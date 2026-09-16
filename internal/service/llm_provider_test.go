package service_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/glennprays/catet-backend/config"
	"github.com/glennprays/catet-backend/internal/apperror"
	"github.com/glennprays/catet-backend/internal/llm"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/glennprays/catet-backend/internal/secretbox"
	"github.com/glennprays/catet-backend/internal/service"
	"github.com/glennprays/catet-backend/internal/testkit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// envConfig is the environment half of the BYOK story: what the
// instance booted with, and what it falls back to whenever the
// dashboard has not overridden the provider.
func envConfig() *config.Config {
	return &config.Config{
		AuthSecret:           "test-auth-secret",
		LlmBaseURL:           "https://env.example.test/v1",
		LlmAPIKey:            "sk-env-0000ENV1",
		LlmModel:             "env-model",
		LlmVisionModel:       "env-vision",
		LlmTimeoutSecs:       8,
		LlmVisionTimeoutSecs: 45,
	}
}

func newLLMProviderFixture(t *testing.T) (context.Context, *service.LLMProviderService, *config.Config) {
	t.Helper()
	pool := testkit.NewDB(t)
	ctx, _ := testkit.Scoped(t, pool, "+628111000041", "AI admin")
	cfg := envConfig()
	svc := service.NewLLMProviderService(
		repository.NewLLMProviderRepository(pool),
		cfg,
		secretbox.New(cfg.AuthSecret),
		testkit.Logger(t),
	)
	return ctx, svc, cfg
}

// With no row saved, the environment is authoritative — the pre-BYOK
// behaviour, unchanged.
func TestLLMProviderFallsBackToEnvironment(t *testing.T) {
	ctx, svc, cfg := newLLMProviderFixture(t)

	current := svc.Current(ctx)
	assert.Equal(t, cfg.LlmBaseURL, current.BaseURL)
	assert.Equal(t, cfg.LlmAPIKey, current.APIKey)
	assert.Equal(t, cfg.LlmModel, current.Model)
	assert.Equal(t, cfg.LlmVisionModel, current.VisionModel)
	assert.Equal(t, 8*time.Second, current.Timeout)
	assert.Equal(t, 45*time.Second, current.VisionTimeout)

	got, err := svc.Get(ctx)
	require.NoError(t, err)
	assert.Equal(t, "env", got.Source)
	assert.True(t, got.APIKeySet)
	assert.Equal(t, "ENV1", got.APIKeyLast4, "only the last four ever leave the server")
	assert.True(t, got.Enabled)
}

// Saving from the dashboard overrides the environment on the next
// request — no restart — and the key is never readable back out.
func TestLLMProviderUpdateOverridesEnvironment(t *testing.T) {
	ctx, svc, _ := newLLMProviderFixture(t)
	key := "sk-dash-1234LAST"

	got, err := svc.Update(ctx, service.UpdateLLMProviderInput{
		BaseURL:              "https://dash.example.test/v1/",
		APIKey:               &key,
		Model:                "dash-model",
		VisionModel:          "dash-vision",
		TimeoutSeconds:       20,
		VisionTimeoutSeconds: 60,
	})
	require.NoError(t, err)
	assert.Equal(t, "dashboard", got.Source)
	assert.Equal(t, "https://dash.example.test/v1", got.BaseURL, "the trailing slash is stripped on the way in")
	assert.True(t, got.APIKeySet)
	assert.Equal(t, "LAST", got.APIKeyLast4)
	assert.True(t, got.Enabled)
	assert.Equal(t, 20, got.TimeoutSeconds)
	assert.Equal(t, 60, got.VisionTimeoutSeconds)

	current := svc.Current(ctx)
	assert.Equal(t, "https://dash.example.test/v1", current.BaseURL)
	assert.Equal(t, key, current.APIKey, "the bot gets the plaintext key, the API never does")
	assert.Equal(t, "dash-model", current.Model)
	assert.Equal(t, 20*time.Second, current.Timeout)
	assert.True(t, llm.NewWithSource(svc).Enabled(ctx))
}

// The three states of api_key, which is the whole reason it is a
// pointer: absent keeps, empty clears, present replaces.
func TestLLMProviderKeyTriState(t *testing.T) {
	ctx, svc, _ := newLLMProviderFixture(t)
	key := "sk-dash-1234LAST"
	client := llm.NewWithSource(svc)

	_, err := svc.Update(ctx, service.UpdateLLMProviderInput{
		BaseURL: "https://dash.example.test/v1", APIKey: &key,
		Model: "dash-model", TimeoutSeconds: 8, VisionTimeoutSeconds: 45,
	})
	require.NoError(t, err)

	// Absent: changing only the model must not require re-typing the key.
	got, err := svc.Update(ctx, service.UpdateLLMProviderInput{
		BaseURL: "https://dash.example.test/v1", APIKey: nil,
		Model: "other-model", TimeoutSeconds: 8, VisionTimeoutSeconds: 45,
	})
	require.NoError(t, err)
	assert.Equal(t, "other-model", got.Model)
	assert.True(t, got.APIKeySet)
	assert.Equal(t, "LAST", got.APIKeyLast4)
	assert.Equal(t, key, svc.Current(ctx).APIKey)

	// Empty: AI off, instantly, without the environment answering for it.
	cleared := ""
	got, err = svc.Update(ctx, service.UpdateLLMProviderInput{
		BaseURL: "https://dash.example.test/v1", APIKey: &cleared,
		Model: "other-model", TimeoutSeconds: 8, VisionTimeoutSeconds: 45,
	})
	require.NoError(t, err)
	assert.False(t, got.APIKeySet)
	assert.Empty(t, got.APIKeyLast4)
	assert.False(t, got.Enabled)
	assert.Empty(t, svc.Current(ctx).APIKey)
	assert.False(t, client.Enabled(ctx), "an empty stored key disables the bot's AI")
}

// A first save that names no key inherits the environment's, so the
// admin can repoint the model without holding the secret.
func TestLLMProviderSeedsKeyFromEnvironmentOnFirstSave(t *testing.T) {
	ctx, svc, cfg := newLLMProviderFixture(t)

	got, err := svc.Update(ctx, service.UpdateLLMProviderInput{
		BaseURL: "https://dash.example.test/v1", APIKey: nil,
		Model: "dash-model", TimeoutSeconds: 8, VisionTimeoutSeconds: 45,
	})
	require.NoError(t, err)
	assert.Equal(t, "dashboard", got.Source)
	assert.True(t, got.APIKeySet)
	assert.Equal(t, "ENV1", got.APIKeyLast4)
	assert.Equal(t, cfg.LlmAPIKey, svc.Current(ctx).APIKey)
}

func TestLLMProviderResetReturnsToEnvironment(t *testing.T) {
	ctx, svc, cfg := newLLMProviderFixture(t)
	key := "sk-dash-1234LAST"
	_, err := svc.Update(ctx, service.UpdateLLMProviderInput{
		BaseURL: "https://dash.example.test/v1", APIKey: &key,
		Model: "dash-model", TimeoutSeconds: 20, VisionTimeoutSeconds: 60,
	})
	require.NoError(t, err)

	require.NoError(t, svc.Reset(ctx))

	got, err := svc.Get(ctx)
	require.NoError(t, err)
	assert.Equal(t, "env", got.Source)
	assert.Equal(t, cfg.LlmModel, got.Model)
	assert.Equal(t, cfg.LlmAPIKey, svc.Current(ctx).APIKey)

	// Resetting twice is the same desired state, not an error.
	require.NoError(t, svc.Reset(ctx))
}

// A base URL that is not an absolute http(s) address is a field error,
// not a 502 from some request we should never have made.
func TestLLMProviderRejectsBadBaseURL(t *testing.T) {
	ctx, svc, _ := newLLMProviderFixture(t)

	for _, bad := range []string{"", "example.test/v1", "ftp://example.test/v1", "://nope"} {
		_, err := svc.Update(ctx, service.UpdateLLMProviderInput{
			BaseURL: bad, Model: "m", TimeoutSeconds: 8, VisionTimeoutSeconds: 45,
		})
		appErr := apperror.AsAppError(err)
		require.NotNil(t, appErr, "base_url %q must be refused", bad)
		assert.Equal(t, apperror.KindValidation, appErr.Kind)
		require.Len(t, appErr.Details, 1)
		assert.Equal(t, "base_url", appErr.Details[0].Field)
	}
}

// The model picker reads the provider's own catalogue, sorted, with the
// key it would actually use.
func TestLLMProviderListModels(t *testing.T) {
	ctx, svc, cfg := newLLMProviderFixture(t)

	var authorization, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"zeta"},{"id":"alpha"},{"id":""}]}`))
	}))
	defer srv.Close()

	got, err := svc.ListModels(ctx, srv.URL+"/", nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"alpha", "zeta"}, got.Models, "sorted, empty ids dropped")
	assert.Equal(t, "/models", path)
	assert.Equal(t, "Bearer "+cfg.LlmAPIKey, authorization, "an absent key falls back to the stored one, then the environment")

	draft := "sk-draft-9999"
	_, err = svc.ListModels(ctx, srv.URL, &draft)
	require.NoError(t, err)
	assert.Equal(t, "Bearer "+draft, authorization, "the draft key wins so a provider can be tried before it is saved")
}

// A provider that errors, or answers with something that is not a model
// list, is an upstream failure (502) — never a 500 blamed on us.
func TestLLMProviderListModelsUpstreamFailures(t *testing.T) {
	ctx, svc, _ := newLLMProviderFixture(t)

	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	}))
	defer refusing.Close()

	_, err := svc.ListModels(ctx, refusing.URL, nil)
	appErr := apperror.AsAppError(err)
	require.NotNil(t, appErr)
	assert.Equal(t, apperror.KindUpstream, appErr.Kind)
	assert.Equal(t, http.StatusBadGateway, appErr.HTTPStatus())
	assert.Contains(t, appErr.Message, "invalid api key")

	babbling := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html>not a model list</html>`))
	}))
	defer babbling.Close()

	_, err = svc.ListModels(ctx, babbling.URL, nil)
	appErr = apperror.AsAppError(err)
	require.NotNil(t, appErr)
	assert.Equal(t, apperror.KindUpstream, appErr.Kind)
}

// The connection test proves a draft triple works and reports the
// provider's own complaint when it does not.
func TestLLMProviderTest(t *testing.T) {
	ctx, svc, _ := newLLMProviderFixture(t)

	var body map[string]any
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"ok\":true}"}}]}`))
	}))
	defer ok.Close()

	draft := "sk-draft-9999"
	got, err := svc.Test(ctx, service.UpdateLLMProviderInput{
		BaseURL: ok.URL, APIKey: &draft, Model: "draft-model",
		TimeoutSeconds: 8, VisionTimeoutSeconds: 45,
	})
	require.NoError(t, err)
	assert.True(t, got.OK)
	assert.Equal(t, "draft-model", got.Model)
	assert.GreaterOrEqual(t, got.LatencyMs, int64(0))
	assert.Equal(t, "draft-model", body["model"], "the test must exercise the model the admin typed")

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"model not found"}}`))
	}))
	defer bad.Close()

	_, err = svc.Test(ctx, service.UpdateLLMProviderInput{
		BaseURL: bad.URL, APIKey: &draft, Model: "nope",
		TimeoutSeconds: 8, VisionTimeoutSeconds: 45,
	})
	appErr := apperror.AsAppError(err)
	require.NotNil(t, appErr)
	assert.Equal(t, apperror.KindUpstream, appErr.Kind)
	assert.Contains(t, appErr.Message, "model not found")

	// Nothing to test is the admin's mistake, not the provider's.
	cleared := ""
	_, err = svc.Update(ctx, service.UpdateLLMProviderInput{
		BaseURL: ok.URL, APIKey: &cleared, Model: "draft-model",
		TimeoutSeconds: 8, VisionTimeoutSeconds: 45,
	})
	require.NoError(t, err)
	_, err = svc.Test(ctx, service.UpdateLLMProviderInput{
		BaseURL: ok.URL, Model: "draft-model",
		TimeoutSeconds: 8, VisionTimeoutSeconds: 45,
	})
	appErr = apperror.AsAppError(err)
	require.NotNil(t, appErr)
	assert.Equal(t, apperror.KindBadRequest, appErr.Kind)
}

// AUTH_SECRET rotated: the stored key is unrecoverable, so AI goes off
// and the admin is told to re-enter it — the instance must not crash or
// silently fall back to an environment key the dashboard replaced.
func TestLLMProviderUndecryptableKeyDisablesAI(t *testing.T) {
	pool := testkit.NewDB(t)
	ctx, _ := testkit.Scoped(t, pool, "+628111000042", "AI admin rotated")
	repo := repository.NewLLMProviderRepository(pool)
	cfg := envConfig()

	key := "sk-dash-1234LAST"
	_, err := service.NewLLMProviderService(repo, cfg, secretbox.New("old-secret"), testkit.Logger(t)).
		Update(ctx, service.UpdateLLMProviderInput{
			BaseURL: "https://dash.example.test/v1", APIKey: &key,
			Model: "dash-model", TimeoutSeconds: 8, VisionTimeoutSeconds: 45,
		})
	require.NoError(t, err)

	rotated := service.NewLLMProviderService(repo, cfg, secretbox.New("new-secret"), testkit.Logger(t))
	assert.Empty(t, rotated.Current(ctx).APIKey, "an unopenable key is AI off, not a panic")
	assert.False(t, llm.NewWithSource(rotated).Enabled(ctx))

	got, err := rotated.Get(ctx)
	require.NoError(t, err)
	assert.Equal(t, "dashboard", got.Source)
	assert.True(t, got.APIKeySet, "a key is installed; it just cannot be opened any more")

	_, err = rotated.ListModels(ctx, "https://dash.example.test/v1", nil)
	appErr := apperror.AsAppError(err)
	require.NotNil(t, appErr)
	assert.Equal(t, apperror.KindBadRequest, appErr.Kind)
	assert.Contains(t, appErr.Message, "enter it again")
}
