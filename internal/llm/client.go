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

	"github.com/glennprays/catet-backend/config"
	"github.com/google/uuid"
)

// ErrUnavailable is returned by every method of a nil *Client. Provide
// returns nil when no API key is configured, so callers can hold the
// client unconditionally and branch on this error instead of scattering
// nil checks.
var ErrUnavailable = errors.New("llm unavailable")

// Candidate is one whitelisted choice offered to the model. Hint is the
// parent category name for categories and the source type for sources —
// enough disambiguation without leaking the whole row.
type Candidate struct {
	ID   uuid.UUID
	Name string
	Hint string
}

// Client talks to an OpenAI-compatible /chat/completions endpoint. The
// vision pair is a second (model, timeout) for image requests: reading a
// receipt photo is a different model class and an order of magnitude
// slower than a category pick.
type Client struct {
	baseURL       string
	apiKey        string
	model         string
	timeout       time.Duration
	visionModel   string
	visionTimeout time.Duration
	http          *http.Client
}

// defaultTimeout mirrors config's LLM_TIMEOUT_SECONDS default and keeps
// a hand-built client from hanging forever.
const defaultTimeout = 8 * time.Second

// maxAttempts is the whole retry policy: one extra try for a transient
// failure, never more — a WhatsApp reply that arrives late is worse than
// an uncategorised write.
const maxAttempts = 2

func New(baseURL, apiKey, model string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &Client{
		baseURL:       strings.TrimRight(baseURL, "/"),
		apiKey:        apiKey,
		model:         model,
		timeout:       timeout,
		visionModel:   model,
		visionTimeout: timeout,
		// No client-level timeout: the per-attempt deadline rides on the
		// request context so the retry budget stays in one place.
		http: &http.Client{},
	}
}

// Provide returns nil when no API key is set. A nil client is the
// documented "LLM disabled" state, not a bug.
func Provide(cfg *config.Config) *Client {
	if cfg == nil || cfg.LlmAPIKey == "" {
		return nil
	}
	return New(cfg.LlmBaseURL, cfg.LlmAPIKey, cfg.LlmModel, time.Duration(cfg.LlmTimeoutSecs)*time.Second).
		WithVision(cfg.LlmVisionModel, time.Duration(cfg.LlmVisionTimeoutSecs)*time.Second)
}

// WithVision points image requests at their own model and timeout. An
// empty model or a non-positive timeout keeps the text defaults, so a
// deployment without LLM_VISION_MODEL still reads photos with whatever
// LLM_MODEL is.
func (c *Client) WithVision(model string, timeout time.Duration) *Client {
	if c == nil {
		return nil
	}
	if strings.TrimSpace(model) != "" {
		c.visionModel = model
	}
	if timeout > 0 {
		c.visionTimeout = timeout
	}
	return c
}

// PickCategory chooses one category id for a transaction. An id the
// model made up, or one missing from cats, is read as "no answer".
func (c *Client) PickCategory(ctx context.Context, merchant, note, typ string, cats []Candidate) (*uuid.UUID, float64, error) {
	if c == nil {
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
	if c == nil {
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

// verdictBanned rejects any verdict that quotes a figure: the numbers in
// a rekap come from the ledger, never from the model.
var verdictBanned = regexp.MustCompile(`[0-9]|Rp|\brb\b|\bjt\b|\bk\b`)

// verdictMaxRunes keeps the verdict to one glanceable sentence.
const verdictMaxRunes = 90

// Verdict returns one short sentence of commentary, or "" when the model
// broke the no-numbers/short-sentence contract. A rejected verdict is
// not an error — the rekap simply ships without it.
func (c *Client) Verdict(ctx context.Context, lang string, facts map[string]string) (string, error) {
	if c == nil {
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
	Usage *usage `json:"usage"`
}

// usage is the token accounting the provider reports. Photo reads are
// the only expensive call in this system, so their cost is logged.
type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// complete runs one guarded text call against the text model, ignoring
// token accounting.
func (c *Client) complete(ctx context.Context, system, user, schemaName string, schema map[string]any, out any) error {
	_, err := c.completeWith(ctx, c.model, c.timeout, []chatMessage{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	}, schemaName, schema, out)
	return err
}

// completeWith runs one guarded call: at most maxAttempts attempts, each
// bounded by timeout, all of them inside a 2x budget so a hung endpoint
// can never hold a WhatsApp reply longer than that. The system message
// (messages[0], by convention) gets the schema appended.
func (c *Client) completeWith(
	ctx context.Context,
	model string,
	timeout time.Duration,
	messages []chatMessage,
	schemaName string,
	schema map[string]any,
	out any,
) (usage, error) {
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
		content, u, retryable, err := c.attempt(budget, timeout, body)
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
// a 4xx is our own fault and never is. The usage is nil unless the
// provider reported it.
func (c *Client) attempt(budget context.Context, timeout time.Duration, body []byte) (string, *usage, bool, error) {
	ctx, cancel := context.WithTimeout(budget, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", nil, false, fmt.Errorf("llm: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

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
	return parsed.Choices[0].Message.Content, parsed.Usage, false, nil
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
