package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/glennprays/catet-backend/config"
	"github.com/glennprays/catet-backend/internal/apperror"
	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/llm"
	"github.com/glennprays/catet-backend/internal/params/response"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/glennprays/catet-backend/internal/scope"
	"github.com/glennprays/catet-backend/internal/secretbox"
	"github.com/glennprays/catet-backend/pkg/logctx"
	"github.com/glennprays/log"
)

// LLMProviderService is the admin's AI provider: the dashboard's
// singleton row, the environment as its fallback, and the key sealed at
// rest in between.
//
// It is also the bot's llm.SettingsSource, which is what makes a saved
// change take effect without a restart: the client asks this service
// for the provider on every request. There is deliberately no cache —
// one primary-key read is nothing next to the completion that follows
// it, and caching would need an invalidation path across the HTTP and
// bot halves of the same process for no measurable gain.
type LLMProviderService struct {
	repo *repository.LLMProviderRepository
	cfg  *config.Config
	box  secretbox.Box
	log  *log.Logger

	// The two failures that can happen on the bot's hot path are both
	// operator problems, not per-message ones: a rotated AUTH_SECRET and
	// a database that will not answer. They are reported once so a
	// broken instance does not also flood its own logs.
	decryptOnce sync.Once
	readOnce    sync.Once

	// http serves the provider catalogue (GET /models). The connection
	// test goes through llm.Client instead, so the test exercises the
	// same request path the bot uses.
	http *http.Client
}

func NewLLMProviderService(
	repo *repository.LLMProviderRepository,
	cfg *config.Config,
	box secretbox.Box,
	logger *log.Logger,
) *LLMProviderService {
	return &LLMProviderService{
		repo: repo,
		cfg:  cfg,
		box:  box,
		log:  logger,
		http: &http.Client{},
	}
}

// modelsTimeout bounds the catalogue fetch. It is the admin waiting in
// front of a dropdown, not a WhatsApp reply, so it can be generous.
const modelsTimeout = 10 * time.Second

// Current implements llm.SettingsSource: the provider as it stands right
// now. Every failure degrades to something safe — the environment when
// the row cannot be read, AI off when the stored key cannot be opened —
// because the alternative is a bot that answers nothing at all.
func (s *LLMProviderService) Current(ctx context.Context) llm.Settings {
	row, err := s.repo.Get(ctx)
	if err != nil {
		s.readOnce.Do(func() {
			s.log.Error(llmTraceOf(ctx), "llm provider row unreadable; falling back to environment", map[string]any{"error": err.Error()})
		})
		return llm.EnvSettings(s.cfg)
	}
	if row == nil {
		return llm.EnvSettings(s.cfg)
	}

	key, err := s.box.Open(row.APIKeyEnc)
	if err != nil {
		// AUTH_SECRET rotated (or the column was edited): the key is
		// unrecoverable and the admin must re-enter it. An empty key is
		// the documented AI-off state, so the bot degrades to rules.
		s.decryptOnce.Do(func() {
			s.log.Error(llmTraceOf(ctx), "stored llm api key cannot be decrypted; AI is off until it is re-entered", map[string]any{"error": err.Error()})
		})
		key = ""
	}
	return llm.Settings{
		BaseURL:       row.BaseURL,
		APIKey:        key,
		Model:         row.Model,
		VisionModel:   row.VisionModel,
		Timeout:       time.Duration(row.TimeoutSeconds) * time.Second,
		VisionTimeout: time.Duration(row.VisionTimeoutSeconds) * time.Second,
	}
}

// Get is the dashboard's read. The key itself is never in the answer:
// only whether one is installed and its last four characters, so the
// admin can tell which key is in place without it being readable back.
func (s *LLMProviderService) Get(ctx context.Context) (*response.LLMProviderData, error) {
	row, err := s.repo.Get(ctx)
	if err != nil {
		return nil, apperror.Internal("failed to load llm provider").Wrap(err)
	}
	if row == nil {
		set := s.cfg.LlmAPIKey != ""
		return &response.LLMProviderData{
			Source:               "env",
			BaseURL:              s.cfg.LlmBaseURL,
			Model:                s.cfg.LlmModel,
			VisionModel:          s.cfg.LlmVisionModel,
			TimeoutSeconds:       s.cfg.LlmTimeoutSecs,
			VisionTimeoutSeconds: s.cfg.LlmVisionTimeoutSecs,
			APIKeySet:            set,
			APIKeyLast4:          last4(s.cfg.LlmAPIKey),
			Enabled:              set,
		}, nil
	}
	set := row.APIKeyEnc != ""
	return &response.LLMProviderData{
		Source:               "dashboard",
		BaseURL:              row.BaseURL,
		Model:                row.Model,
		VisionModel:          row.VisionModel,
		TimeoutSeconds:       row.TimeoutSeconds,
		VisionTimeoutSeconds: row.VisionTimeoutSeconds,
		APIKeySet:            set,
		APIKeyLast4:          row.APIKeyLast4,
		Enabled:              set,
	}, nil
}

// UpdateLLMProviderInput is the parsed PUT /admin/llm body. APIKey is
// tri-state: nil keeps the stored key, "" clears it (AI off), anything
// else replaces it.
type UpdateLLMProviderInput struct {
	BaseURL              string
	APIKey               *string
	Model                string
	VisionModel          string
	TimeoutSeconds       int
	VisionTimeoutSeconds int
}

// Update writes the dashboard override. Saving while the environment is
// still authoritative and without naming a key seeds the stored key
// from the environment: changing only the model must not require the
// admin to re-type a secret this API never shows back.
func (s *LLMProviderService) Update(ctx context.Context, in UpdateLLMProviderInput) (*response.LLMProviderData, error) {
	base, err := normalizeBaseURL(in.BaseURL)
	if err != nil {
		return nil, err
	}

	row, err := s.repo.Get(ctx)
	if err != nil {
		return nil, apperror.Internal("failed to load llm provider").Wrap(err)
	}

	enc, l4 := "", ""
	switch {
	case in.APIKey != nil:
		enc, err = s.box.Seal(*in.APIKey)
		if err != nil {
			return nil, apperror.Internal("failed to seal api key").Wrap(err)
		}
		l4 = last4(*in.APIKey)
	case row != nil:
		enc, l4 = row.APIKeyEnc, row.APIKeyLast4
	default:
		enc, err = s.box.Seal(s.cfg.LlmAPIKey)
		if err != nil {
			return nil, apperror.Internal("failed to seal api key").Wrap(err)
		}
		l4 = last4(s.cfg.LlmAPIKey)
	}

	next := entity.LLMProvider{
		BaseURL:              base,
		APIKeyEnc:            enc,
		APIKeyLast4:          l4,
		Model:                strings.TrimSpace(in.Model),
		VisionModel:          strings.TrimSpace(in.VisionModel),
		TimeoutSeconds:       in.TimeoutSeconds,
		VisionTimeoutSeconds: in.VisionTimeoutSeconds,
	}
	// The admin is the audit trail, not an authorisation check: the
	// route is already admin-only. A context without a user (the
	// operator CLI) simply leaves the column NULL.
	if uid, err := scope.UserID(ctx); err == nil {
		next.UpdatedBy = &uid
	}
	if err := s.repo.Upsert(ctx, next); err != nil {
		return nil, apperror.Internal("failed to save llm provider").Wrap(err)
	}
	return s.Get(ctx)
}

// Reset drops the dashboard override, putting the instance back on the
// environment variables it booted with.
func (s *LLMProviderService) Reset(ctx context.Context) error {
	if err := s.repo.Delete(ctx); err != nil {
		return apperror.Internal("failed to reset llm provider").Wrap(err)
	}
	return nil
}

// ListModels asks a provider what it serves so the dashboard can offer
// a picker. It runs against the *draft* base URL and key, before
// anything is saved; an absent key falls back to the stored one and
// then to the environment.
func (s *LLMProviderService) ListModels(ctx context.Context, baseURL string, apiKey *string) (*response.LLMModelsData, error) {
	base, err := normalizeBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	key, err := s.resolveKey(ctx, apiKey)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, modelsTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/models", nil)
	if err != nil {
		return nil, apperror.BadRequest("invalid base url").Wrap(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	resp, err := s.http.Do(req)
	if err != nil {
		return nil, apperror.BadGateway("provider unreachable").Wrap(err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, apperror.BadGateway("provider response unreadable").Wrap(err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, apperror.BadGatewayf("provider answered %d: %s", resp.StatusCode, upstreamSnippet(raw))
	}

	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, apperror.BadGatewayf("provider did not answer with a model list: %s", upstreamSnippet(raw)).Wrap(err)
	}

	models := make([]string, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		if id := strings.TrimSpace(m.ID); id != "" {
			models = append(models, id)
		}
	}
	sort.Strings(models)
	return &response.LLMModelsData{Models: models}, nil
}

// Test proves a draft provider is a working triple (base URL, key,
// model) before the admin commits to it. The provider's own error text
// is passed through: "model not found" is the whole answer the admin
// needs, and paraphrasing it would only hide it.
func (s *LLMProviderService) Test(ctx context.Context, in UpdateLLMProviderInput) (*response.LLMTestData, error) {
	base, err := normalizeBaseURL(in.BaseURL)
	if err != nil {
		return nil, err
	}
	key, err := s.resolveKey(ctx, in.APIKey)
	if err != nil {
		return nil, err
	}
	if key == "" {
		return nil, apperror.BadRequest("no api key to test; enter one first")
	}
	model := strings.TrimSpace(in.Model)
	if model == "" {
		return nil, apperror.BadRequest("no model to test")
	}

	client := llm.New(base, key, model, time.Duration(in.TimeoutSeconds)*time.Second)
	start := time.Now()
	if err := client.Ping(ctx); err != nil {
		return nil, apperror.BadGateway(err.Error()).Wrap(err)
	}
	return &response.LLMTestData{
		OK:        true,
		LatencyMs: time.Since(start).Milliseconds(),
		Model:     model,
	}, nil
}

// resolveKey implements the one key-resolution rule shared by the model
// list and the connection test: what the admin typed, else what is
// stored, else what the environment holds. An empty typed key is "not
// typed" — clearing the key is Update's job, not a test's.
func (s *LLMProviderService) resolveKey(ctx context.Context, given *string) (string, error) {
	if given != nil && strings.TrimSpace(*given) != "" {
		return *given, nil
	}
	row, err := s.repo.Get(ctx)
	if err != nil {
		return "", apperror.Internal("failed to load llm provider").Wrap(err)
	}
	if row != nil && row.APIKeyEnc != "" {
		key, err := s.box.Open(row.APIKeyEnc)
		if err != nil {
			return "", apperror.BadRequest("the stored api key cannot be decrypted; enter it again")
		}
		return key, nil
	}
	if row != nil {
		// The dashboard cleared the key on purpose: AI is off, and the
		// environment must not quietly answer for it.
		return "", nil
	}
	return s.cfg.LlmAPIKey, nil
}

// normalizeBaseURL is the one shape rule for a provider address: an
// absolute http(s) URL, trailing slash removed so every caller can
// append "/chat/completions" or "/models" without thinking about it.
func normalizeBaseURL(raw string) (string, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(raw), "/")
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", apperror.Validation("base_url must be an absolute http or https url").WithDetails(apperror.FieldError{
			Field:   "base_url",
			Rule:    "url",
			Message: "must be an absolute http or https url",
		})
	}
	return trimmed, nil
}

// last4 is the only fragment of an API key that ever leaves the server.
// A key too short to have four characters yields nothing rather than
// most of itself.
func last4(key string) string {
	if len(key) < 4 {
		return ""
	}
	return key[len(key)-4:]
}

// upstreamSnippet keeps a provider's error readable when it answers
// with HTML or a wall of JSON.
func upstreamSnippet(b []byte) string {
	const limit = 200
	s := strings.TrimSpace(string(b))
	if len(s) > limit {
		return s[:limit]
	}
	return s
}

// Compile-time proof that the bot's client can be pointed at this
// service: the whole no-restart story depends on it.
var _ llm.SettingsSource = (*LLMProviderService)(nil)

// llmTraceOf correlates a log line with the request that caused it. The
// bot's hot path often has no trace id (a bot message is not an HTTP
// request) and the logger refuses an empty one.
func llmTraceOf(ctx context.Context) string {
	if id := logctx.TraceID(ctx); id != "" {
		return id
	}
	return "llm-provider"
}
