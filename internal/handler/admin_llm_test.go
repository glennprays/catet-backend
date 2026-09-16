package handler_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/glennprays/catet-backend/config"
	"github.com/glennprays/catet-backend/internal/auth"
	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/handler"
	"github.com/glennprays/catet-backend/internal/params/response"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/glennprays/catet-backend/internal/secretbox"
	"github.com/glennprays/catet-backend/internal/service"
	"github.com/glennprays/catet-backend/internal/testkit"
	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type adminLLMFixture struct {
	app    *fiber.App
	users  *repository.UserRepository
	tokens *auth.TokenService
	pool   *pgxpool.Pool
}

// newAdminLLMFixture mounts /admin/llm the way internal/router does:
// RequireAuth on the protected group, RequireAdmin on /admin. The gate
// is half the point of these tests, so it is the real chain.
func newAdminLLMFixture(t *testing.T) *adminLLMFixture {
	t.Helper()
	pool := testkit.NewDB(t)

	cfg := &config.Config{
		AuthSecret:           "test-auth-secret",
		LlmBaseURL:           "https://env.example.test/v1",
		LlmAPIKey:            "sk-env-0000ENV1",
		LlmModel:             "env-model",
		LlmTimeoutSecs:       8,
		LlmVisionTimeoutSecs: 45,
	}
	users := repository.NewUserRepository(pool)
	tokens := auth.NewTokenService("test-secret")
	svc := service.NewLLMProviderService(
		repository.NewLLMProviderRepository(pool),
		cfg,
		secretbox.New(cfg.AuthSecret),
		testkit.Logger(t),
	)
	h := handler.NewAdminLLMHandler(svc)

	app := testkit.NewTestApp(t, func(app *fiber.App) {
		v1 := app.Group("/api/v1")
		protected := v1.Group("", auth.RequireAuth(tokens, users))
		admin := protected.Group("/admin", auth.RequireAdmin)
		admin.Get("/llm", h.Get)
		admin.Put("/llm", h.Update)
		admin.Delete("/llm", h.Reset)
		admin.Post("/llm/models", h.Models)
		admin.Post("/llm/test", h.Test)
	})
	return &adminLLMFixture{app: app, users: users, tokens: tokens, pool: pool}
}

func (f *adminLLMFixture) token(t *testing.T, phone string, isAdmin bool) string {
	t.Helper()
	u, err := f.users.Create(t.Context(), &entity.User{
		Phone: phone, DisplayName: phone, IsAdmin: isAdmin,
	})
	require.NoError(t, err)
	token, _, err := f.tokens.Issue(u.ID, u.SessionEpoch)
	require.NoError(t, err)
	return token
}

func (f *adminLLMFixture) do(t *testing.T, method, path, token, body string) *http.Response {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	}
	req.Header.Set(fiber.HeaderAuthorization, "Bearer "+token)
	resp, err := f.app.Test(req)
	require.NoError(t, err)
	return resp
}

// The provider is instance-wide configuration — one key, one bill — so
// only the admin may read or change it.
func TestAdminLLMHandler_NonAdminIsForbidden(t *testing.T) {
	f := newAdminLLMFixture(t)
	token := f.token(t, "+628111000051", false)

	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/admin/llm", ""},
		{http.MethodPut, "/api/v1/admin/llm", `{"base_url":"https://x.test/v1","model":"m","timeout_seconds":8,"vision_timeout_seconds":45}`},
		{http.MethodDelete, "/api/v1/admin/llm", ""},
		{http.MethodPost, "/api/v1/admin/llm/models", `{"base_url":"https://x.test/v1"}`},
		{http.MethodPost, "/api/v1/admin/llm/test", `{"base_url":"https://x.test/v1","model":"m","timeout_seconds":8,"vision_timeout_seconds":45}`},
	} {
		resp := f.do(t, tc.method, tc.path, token, tc.body)
		assert.Equal(t, http.StatusForbidden, resp.StatusCode, "%s %s", tc.method, tc.path)
		_, code := decodeEnvelope[response.LLMProviderData](t, resp)
		assert.Equal(t, "FORBIDDEN", code)
	}
}

// The admin's round trip over HTTP: env by default, dashboard after a
// save (key never echoed), env again after a reset.
func TestAdminLLMHandler_SaveAndReset(t *testing.T) {
	f := newAdminLLMFixture(t)
	token := f.token(t, "+628111000052", true)

	resp := f.do(t, http.MethodGet, "/api/v1/admin/llm", token, "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	got, code := decodeEnvelope[response.LLMProviderData](t, resp)
	require.Empty(t, code)
	assert.Equal(t, "env", got.Source)
	assert.Equal(t, "ENV1", got.APIKeyLast4)

	resp = f.do(t, http.MethodPut, "/api/v1/admin/llm", token,
		`{"base_url":"https://dash.example.test/v1","api_key":"sk-dash-1234LAST","model":"dash-model","timeout_seconds":20,"vision_timeout_seconds":60}`)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	got, _ = decodeEnvelope[response.LLMProviderData](t, resp)
	assert.Equal(t, "dashboard", got.Source)
	assert.Equal(t, "LAST", got.APIKeyLast4)
	assert.True(t, got.Enabled)
	assert.Equal(t, 20, got.TimeoutSeconds)

	// A body that breaks the validator is a 400 with field details, not
	// a request to some half-configured provider.
	resp = f.do(t, http.MethodPut, "/api/v1/admin/llm", token,
		`{"base_url":"https://dash.example.test/v1","model":"dash-model","timeout_seconds":900,"vision_timeout_seconds":60}`)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	_, code = decodeEnvelope[response.LLMProviderData](t, resp)
	assert.Equal(t, "VALIDATION_FAILED", code)

	resp = f.do(t, http.MethodDelete, "/api/v1/admin/llm", token, "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	got, _ = decodeEnvelope[response.LLMProviderData](t, resp)
	assert.Equal(t, "env", got.Source)
	assert.Equal(t, "env-model", got.Model)
}

// A provider that cannot be reached is the provider's failure: 502, not
// a 500 that looks like our bug.
func TestAdminLLMHandler_UnreachableProviderIs502(t *testing.T) {
	f := newAdminLLMFixture(t)
	token := f.token(t, "+628111000053", true)

	// Port 0 never accepts a connection, so no real network is touched.
	resp := f.do(t, http.MethodPost, "/api/v1/admin/llm/models", token, `{"base_url":"http://127.0.0.1:0/v1"}`)
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
	_, code := decodeEnvelope[response.LLMModelsData](t, resp)
	assert.Equal(t, "UPSTREAM_ERROR", code)
}
