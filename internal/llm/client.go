// Package llm is a thin OpenAI-compatible chat-completions client for
// the three guesses the bot is allowed to make: which category a
// transaction belongs to, which Source an alias meant, and the optional
// one-line rekap verdict. Every answer is constrained by a strict JSON
// schema and re-checked against the caller's whitelist — the model may
// only pick from ids we handed it, never invent one (ADR-0003).
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrUnavailable is returned by every method of a nil *Client and by
// every method of a client whose provider has no API key. AI is an
// optional dependency: callers hold the client unconditionally and
// branch on this error instead of scattering nil checks.
var ErrUnavailable = errors.New("llm unavailable")

// Candidate is one whitelisted choice offered to the model. Hint is the
// parent category name for categories and the source type for sources —
// enough disambiguation without leaking the whole row.
type Candidate struct {
	ID   uuid.UUID
	Name string
	Hint string
}

// Client talks to an OpenAI-compatible /chat/completions endpoint. It
// holds no provider configuration of its own: the admin can repoint the
// instance from the dashboard between two messages, so every request
// resolves the provider from source first (see settings.go).
type Client struct {
	source SettingsSource
	http   *http.Client
}

// defaultTimeout mirrors config's LLM_TIMEOUT_SECONDS default and keeps
// a client whose settings carry no timeout from hanging forever.
const defaultTimeout = 8 * time.Second

// defaultVisionTimeout mirrors LLM_VISION_TIMEOUT_SECONDS: reading a
// receipt photo is an order of magnitude slower than a category pick.
const defaultVisionTimeout = 45 * time.Second

// maxAttempts is the whole retry policy: one extra try for a transient
// failure, never more — a WhatsApp reply that arrives late is worse than
// an uncategorised write.
const maxAttempts = 2

// New builds a client pinned to one provider. The vision pair defaults
// to the text one; WithVision overrides it.
func New(baseURL, apiKey, model string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return NewWithSource(&staticSource{settings: Settings{
		BaseURL:       baseURL,
		APIKey:        apiKey,
		Model:         model,
		VisionModel:   model,
		Timeout:       timeout,
		VisionTimeout: timeout,
	}})
}

// NewWithSource builds a client whose provider is resolved per request.
// This is the production constructor: source is the service that reads
// the dashboard's singleton row and falls back to the environment.
func NewWithSource(source SettingsSource) *Client {
	return &Client{
		source: source,
		// No client-level timeout: the per-attempt deadline rides on the
		// request context so the retry budget stays in one place.
		http: &http.Client{},
	}
}

// WithVision points image requests at their own model and timeout. An
// empty model or a non-positive timeout keeps the text defaults, so a
// deployment without LLM_VISION_MODEL still reads photos with whatever
// LLM_MODEL is. It only affects a client built by New: a client whose
// provider comes from the dashboard carries the vision pair in its
// settings instead.
func (c *Client) WithVision(model string, timeout time.Duration) *Client {
	if c == nil {
		return nil
	}
	static, ok := c.source.(*staticSource)
	if !ok {
		return c
	}
	if strings.TrimSpace(model) != "" {
		static.settings.VisionModel = model
	}
	if timeout > 0 {
		static.settings.VisionTimeout = timeout
	}
	return c
}

// Enabled reports whether the provider currently configured has an API
// key. A nil client — the wiring's "no bot" case — is never enabled.
// Every exported method refuses when this is false, so the bot's call
// sites ask this instead of comparing the client to nil.
func (c *Client) Enabled(ctx context.Context) bool {
	if c == nil || c.source == nil {
		return false
	}
	return strings.TrimSpace(c.source.Current(ctx).APIKey) != ""
}

// RequestTimeout reports the normalised text-request timeout currently
// configured for the provider. It is safe on a nil client and returns only
// the duration, never provider settings such as the API key.
func (c *Client) RequestTimeout(ctx context.Context) time.Duration {
	return c.resolve(ctx).timeout
}

// PickCategory chooses one category id for a transaction. An id the
// model made up, or one missing from cats, is read as "no answer".
func (c *Client) PickCategory(ctx context.Context, merchant, note, typ string, cats []Candidate) (*uuid.UUID, float64, error) {
	if !c.Enabled(ctx) {
		return nil, 0, ErrUnavailable
	}
	if len(cats) == 0 {
		return nil, 0, nil
	}

	system := "You label Indonesian personal-finance transactions. Choose exactly one category id from the list or null. Never invent ids."

	var user strings.Builder
	user.WriteString("Transaction type: " + typ + "\n")
	user.WriteString("Merchant: " + fallback(merchant, "(unknown)") + "\n")
	if strings.TrimSpace(note) != "" {
		user.WriteString("Note: " + note + "\n")
	}
	user.WriteString("Categories:\n")
	writeCandidates(&user, cats)
	user.WriteString("\nAnswer with the category_id that best fits, or null when none does. confidence is your certainty between 0 and 1.")

	schema := objectSchema(map[string]any{
		"category_id": map[string]any{"type": []string{"string", "null"}},
		"confidence":  map[string]any{"type": "number"},
	}, "category_id", "confidence")

	var out struct {
		CategoryID *string `json:"category_id"`
		Confidence float64 `json:"confidence"`
	}
	if err := c.complete(ctx, system, user.String(), "category_pick", schema, &out); err != nil {
		return nil, 0, err
	}

	id, ok := match(out.CategoryID, cats)
	if !ok {
		return nil, 0, nil
	}
	conf := out.Confidence
	if conf < 0 {
		conf = 0
	} else if conf > 1 {
		conf = 1
	}
	return id, conf, nil
}

// PickSource resolves a typed alias ("spay", "bca") to one Source id.
func (c *Client) PickSource(ctx context.Context, hint string, sources []Candidate) (*uuid.UUID, error) {
	if !c.Enabled(ctx) {
		return nil, ErrUnavailable
	}
	if len(sources) == 0 {
		return nil, nil
	}

	system := "You match a short Indonesian WhatsApp alias to exactly one payment source id from the list, or null when nothing clearly matches. " +
		"Be typo-tolerant and abbreviation-tolerant: 'bri' -> 'BRI', 'spay' -> 'SPayLater', 'gopay' -> 'GoPay'. Never invent ids."

	var user strings.Builder
	user.WriteString("Alias: " + hint + "\n")
	user.WriteString("Sources:\n")
	writeCandidates(&user, sources)
	user.WriteString("\nAnswer with the matching source_id, or null when the alias is ambiguous.")

	schema := objectSchema(map[string]any{
		"source_id": map[string]any{"type": []string{"string", "null"}},
	}, "source_id")

	var out struct {
		SourceID *string `json:"source_id"`
	}
	if err := c.complete(ctx, system, user.String(), "source_pick", schema, &out); err != nil {
		return nil, err
	}
	id, ok := match(out.SourceID, sources)
	if !ok {
		return nil, nil
	}
	return id, nil
}

// MerchantPick is the model's reading of one raw merchant phrase:
// either an existing candidate (MerchantID set) or a tidied new name.
type MerchantPick struct {
	MerchantID *uuid.UUID
	Name       string
	Confidence float64
}

// PickMerchant maps a typed or printed phrase to a canonical merchant.
// known is the whitelist of existing merchants; an id outside it is
// dropped like everywhere else (ADR-0003), leaving the tidied name.
func (c *Client) PickMerchant(ctx context.Context, raw, note string, known []Candidate) (*MerchantPick, error) {
	if !c.Enabled(ctx) {
		return nil, ErrUnavailable
	}

	system := "You normalise the merchant of one personal-finance entry in Indonesia. " +
		"Given a raw phrase (typed in chat or printed on a receipt) and a list of the user's existing merchants (id - name), answer with the canonical merchant. " +
		"Rules: if the phrase names the same brand or the same recurring concept as an existing merchant, return that merchant_id and its name; otherwise return merchant_id null and a new name. " +
		"A name is the brand or concept only, in Title Case, without branch numbers, locations, legal suffixes (PT, Tbk), payment words or amounts: " +
		"'INDOMARET TUNJUNGAN 2' -> 'Indomaret', 'kopi kenangan tp' -> 'Kopi Kenangan'. " +
		"A descriptive phrase with no brand becomes a short Indonesian concept, 2-3 words, so repeats group together: " +
		"'traktir makan temen' and 'traktir temen' -> 'Traktir Teman', 'bensin motor' -> 'Bensin'. " +
		"Never invent a brand the phrase does not imply. confidence is 0..1 for the match or the new name."

	var user strings.Builder
	user.WriteString("Raw: " + raw + "\n")
	user.WriteString("Note: " + fallback(strings.TrimSpace(note), "(none)") + "\n")
	user.WriteString("Existing merchants (id - name):\n")
	if len(known) == 0 {
		user.WriteString("none\n")
	} else {
		writeCandidates(&user, known)
	}

	schema := objectSchema(map[string]any{
		"merchant_id": map[string]any{"type": []string{"string", "null"}},
		"name":        map[string]any{"type": "string"},
		"confidence":  map[string]any{"type": "number"},
	}, "merchant_id", "name", "confidence")

	var out struct {
		MerchantID *string `json:"merchant_id"`
		Name       string  `json:"name"`
		Confidence float64 `json:"confidence"`
	}
	if err := c.complete(ctx, system, user.String(), "merchant_pick", schema, &out); err != nil {
		return nil, err
	}

	pick := &MerchantPick{Name: strings.TrimSpace(out.Name), Confidence: out.Confidence}
	if id, ok := match(out.MerchantID, known); ok {
		pick.MerchantID = id
	}
	if pick.Confidence < 0 {
		pick.Confidence = 0
	} else if pick.Confidence > 1 {
		pick.Confidence = 1
	}
	return pick, nil
}

// verdictBanned rejects any verdict that quotes a figure: the numbers in
// a rekap come from the ledger, never from the model.
var verdictBanned = regexp.MustCompile(`[0-9]|Rp|\brb\b|\bjt\b|\bk\b`)

// verdictMaxRunes keeps the verdict to one glanceable sentence.
const verdictMaxRunes = 90

// Verdict returns one short sentence of commentary, or "" when the model
// broke the no-numbers/short-sentence contract. A rejected verdict is
// not an error — the rekap simply ships without it.
func (c *Client) Verdict(ctx context.Context, lang string, facts map[string]string) (string, error) {
	if !c.Enabled(ctx) {
		return "", ErrUnavailable
	}

	language := "Indonesian"
	if lang == "en" {
		language = "English"
	}
	system := "You write one short sentence of commentary about a personal-finance summary, in " + language + ". " +
		"Never write digits, currency symbols, amounts, percentages or any money figure. " +
		"Never repeat the facts verbatim. At most twenty words, one sentence, no emoji."

	var user strings.Builder
	user.WriteString("Facts:\n")
	keys := make([]string, 0, len(facts))
	for k := range facts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		user.WriteString("- " + k + ": " + facts[k] + "\n")
	}

	schema := objectSchema(map[string]any{
		"verdict": map[string]any{"type": "string"},
	}, "verdict")

	var out struct {
		Verdict string `json:"verdict"`
	}
	if err := c.complete(ctx, system, user.String(), "verdict", schema, &out); err != nil {
		return "", err
	}

	v := strings.TrimSpace(out.Verdict)
	if v == "" || len([]rune(v)) > verdictMaxRunes || verdictBanned.MatchString(v) {
		return "", nil
	}
	return v, nil
}

// chatRequest is the wire shape of an OpenAI-compatible completion.
type chatRequest struct {
	Model          string         `json:"model"`
	Messages       []chatMessage  `json:"messages"`
	Temperature    float64        `json:"temperature"`
	ResponseFormat responseFormat `json:"response_format"`
}

// chatMessage carries either a plain string (text-only requests) or a
// []contentPart (multimodal: a prompt plus an inline image).
type chatMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// contentPart is one element of a multimodal message.
type contentPart struct {
	Type     string    `json:"type"` // "text" | "image_url"
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

// responseFormat asks for JSON-object mode. JSONSchema is kept for
// endpoints that honour strict schemas and is omitted otherwise.
type responseFormat struct {
	Type       string      `json:"type"`
	JSONSchema *jsonSchema `json:"json_schema,omitempty"`
}

type jsonSchema struct {
	Name   string         `json:"name"`
	Strict bool           `json:"strict"`
	Schema map[string]any `json:"schema"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Model string `json:"model"`
	Usage *usage `json:"usage"`
}

// usage is the token accounting the provider reports. Photo reads are
// the only expensive call in this system, so their cost is logged.
type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	// Model is what the provider says it served; filled by attempt, not
	// decoded.
	Model string `json:"-"`
}

// complete runs one guarded text call against the text model, ignoring
// token accounting.
func (c *Client) complete(ctx context.Context, system, user, schemaName string, schema map[string]any, out any) error {
	_, err := c.completeWith(ctx, false, []chatMessage{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	}, schemaName, schema, out)
	return err
}

// completeWith runs one guarded call: at most maxAttempts attempts, each
// bounded by the resolved timeout, all of them inside a 2x budget so a
// hung endpoint can never hold a WhatsApp reply longer than that. The
// system message (messages[0], by convention) gets the schema appended.
// The provider is resolved once here, so every attempt of one request
// talks to the same endpoint even if the admin saves a new one midway.
func (c *Client) completeWith(
	ctx context.Context,
	vision bool,
	messages []chatMessage,
	schemaName string,
	schema map[string]any,
	out any,
) (usage, error) {
	settings := c.resolve(ctx)
	model, timeout := settings.model, settings.timeout
	if vision {
		model, timeout = settings.visionModel, settings.visionTimeout
	}
	var used usage
	// JSON-object mode plus the schema spelled out in the prompt, rather
	// than OpenAI's strict json_schema: OpenAI-compatible routers
	// (9router/GLM among them) accept the response_format field and then
	// ignore a json_schema, answering in prose. json_object is honoured
	// everywhere, and the whitelist check in Go — not the schema — is
	// what actually keeps the model from inventing an id (ADR-0003).
	shape, err := json.Marshal(schema)
	if err != nil {
		return used, fmt.Errorf("llm: encode schema: %w", err)
	}
	if len(messages) == 0 {
		return used, errors.New("llm: no messages")
	}
	system, ok := messages[0].Content.(string)
	if !ok {
		return used, errors.New("llm: system message must be text")
	}
	messages[0].Content = system +
		"\nReply with a single JSON object only — no prose, no code fence. It must match this JSON schema (" +
		schemaName + "): " + string(shape)

	body, err := json.Marshal(chatRequest{
		Model:          model,
		Messages:       messages,
		Temperature:    0,
		ResponseFormat: responseFormat{Type: "json_object"},
	})
	if err != nil {
		return used, fmt.Errorf("llm: encode request: %w", err)
	}

	budget, cancel := context.WithTimeout(ctx, 2*timeout)
	defer cancel()

	var lastErr error
	for range maxAttempts {
		content, u, retryable, err := c.attempt(budget, settings, timeout, body)
		if u != nil {
			used = *u
		}
		if err == nil {
			payload, ok := extractJSONObject(content)
			if !ok {
				return used, fmt.Errorf("llm: no json object in content: %s", snippet([]byte(content)))
			}
			if err := json.Unmarshal([]byte(payload), out); err != nil {
				return used, fmt.Errorf("llm: decode content: %w", err)
			}
			return used, nil
		}
		lastErr = err
		if !retryable || budget.Err() != nil {
			return used, err
		}
	}
	return used, lastErr
}

// extractJSONObject pulls the JSON object out of a model reply that may
// arrive wrapped in a ```json fence or padded with a sentence.
func extractJSONObject(content string) (string, bool) {
	start := strings.Index(content, "{")
	end := strings.LastIndex(content, "}")
	if start < 0 || end <= start {
		return "", false
	}
	return content[start : end+1], true
}

// attempt performs a single request. The bool reports whether the
// failure is worth one more try: network faults, timeouts and 5xx are;
// a 4xx is our own fault and never is. The usage is never nil on
// success: token counts are zero when the provider reports none, and
// Model is whatever it named.
func (c *Client) attempt(budget context.Context, settings resolved, timeout time.Duration, body []byte) (string, *usage, bool, error) {
	ctx, cancel := context.WithTimeout(budget, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, settings.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", nil, false, fmt.Errorf("llm: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+settings.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", nil, true, fmt.Errorf("llm: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, true, fmt.Errorf("llm: read response: %w", err)
	}
	if resp.StatusCode >= 500 {
		return "", nil, true, fmt.Errorf("llm: status %d: %s", resp.StatusCode, snippet(raw))
	}
	if resp.StatusCode >= 400 {
		return "", nil, false, fmt.Errorf("llm: status %d: %s", resp.StatusCode, snippet(raw))
	}

	// Decode the first JSON value and ignore whatever follows: some
	// routers append an SSE terminator ("data: [DONE]") to a non-stream
	// response, which a whole-body Unmarshal rejects.
	var parsed chatResponse
	if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&parsed); err != nil {
		return "", nil, false, fmt.Errorf("llm: decode response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return "", parsed.Usage, false, errors.New("llm: empty choices")
	}
	u := parsed.Usage
	if u == nil {
		u = &usage{}
	}
	u.Model = parsed.Model
	return parsed.Choices[0].Message.Content, u, false, nil
}

// Ping is the admin's connection test: one minimal completion against
// the configured provider. Any 2xx counts — the point is to prove the
// base URL, key and model name are a working triple, not to grade the
// answer, so no schema is imposed and no content is parsed. The error
// carries the provider's own message so the dashboard can show it.
func (c *Client) Ping(ctx context.Context) error {
	if !c.Enabled(ctx) {
		return ErrUnavailable
	}
	settings := c.resolve(ctx)
	body, err := json.Marshal(pingRequest{
		Model: settings.model,
		Messages: []chatMessage{
			{Role: "system", Content: `Reply with JSON {"ok":true}`},
			{Role: "user", Content: "ping"},
		},
		Temperature: 0,
		MaxTokens:   8,
	})
	if err != nil {
		return fmt.Errorf("llm: encode request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, settings.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, settings.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("llm: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+settings.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("llm: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("llm: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("llm: status %d: %s", resp.StatusCode, snippet(raw))
	}
	return nil
}

// pingRequest is the connection test's wire shape: no response_format,
// because a provider that rejects JSON mode would fail a test that is
// only asking whether the endpoint answers at all.
type pingRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
	MaxTokens   int           `json:"max_tokens"`
}

// objectSchema builds the schema handed to the model: every property
// required, no extras.
func objectSchema(props map[string]any, required ...string) map[string]any {
	return map[string]any{
		"type":                 "object",
		"properties":           props,
		"required":             required,
		"additionalProperties": false,
	}
}

func writeCandidates(b *strings.Builder, cands []Candidate) {
	for _, cand := range cands {
		b.WriteString(cand.ID.String() + " - " + cand.Name)
		if cand.Hint != "" {
			b.WriteString(" (" + cand.Hint + ")")
		}
		b.WriteString("\n")
	}
}

// match enforces the whitelist law: the answer counts only when it is
// one of the ids we offered.
func match(raw *string, cands []Candidate) (*uuid.UUID, bool) {
	if raw == nil {
		return nil, false
	}
	id, err := uuid.Parse(strings.TrimSpace(*raw))
	if err != nil {
		return nil, false
	}
	for _, cand := range cands {
		if cand.ID == id {
			picked := id
			return &picked, true
		}
	}
	return nil, false
}

func fallback(s, alt string) string {
	if strings.TrimSpace(s) == "" {
		return alt
	}
	return s
}

// snippet keeps error messages readable when an endpoint returns HTML.
func snippet(b []byte) string {
	const limit = 200
	s := strings.TrimSpace(string(b))
	if len(s) > limit {
		return s[:limit]
	}
	return s
}
