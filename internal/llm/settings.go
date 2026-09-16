package llm

import (
	"context"
	"strings"
	"time"

	"github.com/glennprays/catet-backend/config"
)

// Settings is everything one request needs to know about the provider.
// It is a value, not a handle: the admin may change the provider between
// two bot messages, so the client resolves a fresh copy per request
// instead of freezing one at construction.
type Settings struct {
	BaseURL       string
	APIKey        string
	Model         string
	VisionModel   string
	Timeout       time.Duration
	VisionTimeout time.Duration
}

// SettingsSource answers "which provider, right now". The dashboard
// implementation (service.LLMProviderService) reads the singleton row
// and falls back to the environment; staticSource is the frozen variant
// used by New and by tests.
type SettingsSource interface {
	Current(ctx context.Context) Settings
}

// staticSource backs New/WithVision: a hand-built client whose provider
// never changes.
type staticSource struct {
	settings Settings
}

func (s *staticSource) Current(context.Context) Settings { return s.settings }

// EnvSettings is the provider configured by the environment — the
// fallback whenever the dashboard has not overridden it.
func EnvSettings(cfg *config.Config) Settings {
	if cfg == nil {
		return Settings{}
	}
	return Settings{
		BaseURL:       cfg.LlmBaseURL,
		APIKey:        cfg.LlmAPIKey,
		Model:         cfg.LlmModel,
		VisionModel:   cfg.LlmVisionModel,
		Timeout:       time.Duration(cfg.LlmTimeoutSecs) * time.Second,
		VisionTimeout: time.Duration(cfg.LlmVisionTimeoutSecs) * time.Second,
	}
}

// resolved is one request's view of Settings with every fallback already
// applied, so no call site repeats them.
type resolved struct {
	baseURL       string
	apiKey        string
	model         string
	visionModel   string
	timeout       time.Duration
	visionTimeout time.Duration
}

// resolve reads the source once per request and normalises it: the base
// URL loses its trailing slash, an unset vision model reuses the text
// model, and a non-positive timeout falls back to the same defaults a
// hand-built client gets.
func (c *Client) resolve(ctx context.Context) resolved {
	var s Settings
	if c != nil && c.source != nil {
		s = c.source.Current(ctx)
	}
	r := resolved{
		baseURL:       strings.TrimRight(strings.TrimSpace(s.BaseURL), "/"),
		apiKey:        s.APIKey,
		model:         s.Model,
		visionModel:   s.VisionModel,
		timeout:       s.Timeout,
		visionTimeout: s.VisionTimeout,
	}
	if strings.TrimSpace(r.visionModel) == "" {
		r.visionModel = r.model
	}
	if r.timeout <= 0 {
		r.timeout = defaultTimeout
	}
	if r.visionTimeout <= 0 {
		r.visionTimeout = defaultVisionTimeout
	}
	return r
}
