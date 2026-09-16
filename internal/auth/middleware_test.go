package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/glennprays/catet-backend/internal/testkit"
	"github.com/gofiber/fiber/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type middlewareFixture struct {
	app    *fiber.App
	users  *repository.UserRepository
	tokens *TokenService
}

// newMiddlewareFixture mounts one protected route behind the real
// RequireAuth chain: the renewal header is set by the middleware, so
// only a live request can show whether it travels.
func newMiddlewareFixture(t *testing.T) *middlewareFixture {
	t.Helper()
	pool := testkit.NewDB(t)
	users := repository.NewUserRepository(pool)
	tokens := NewTokenService("test-secret")

	app := testkit.NewTestApp(t, func(app *fiber.App) {
		protected := app.Group("", RequireAuth(tokens, users))
		protected.Get("/ping", func(c *fiber.Ctx) error { return c.SendString("pong") })
	})
	return &middlewareFixture{app: app, users: users, tokens: tokens}
}

func (f *middlewareFixture) createUser(t *testing.T, phone string) *entity.User {
	t.Helper()
	u, err := f.users.Create(t.Context(), &entity.User{Phone: phone, DisplayName: "Owner"})
	require.NoError(t, err)
	return u
}

func (f *middlewareFixture) ping(t *testing.T, token string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	req.Header.Set(fiber.HeaderAuthorization, "Bearer "+token)
	resp, err := f.app.Test(req)
	require.NoError(t, err)
	return resp
}

// A session that has been in use for longer than RenewAfter must come
// back with a replacement, or a phone left installed for a month gets
// signed out mid-use. The replacement has to name the same account at
// the same epoch — a renewal that changed either would be a privilege
// change, not a renewal — and carry a fresh iat so the clock restarts.
func TestRequireAuth_OldTokenIsRenewed(t *testing.T) {
	f := newMiddlewareFixture(t)
	u := f.createUser(t, "+628123456789")

	old, _, err := f.tokens.issueAt(u.ID, u.SessionEpoch, time.Now().Add(-8*24*time.Hour))
	require.NoError(t, err)

	resp := f.ping(t, old)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	renewed := resp.Header.Get(RenewHeader)
	require.NotEmpty(t, renewed, "a token older than RenewAfter must be answered with a fresh one")
	assert.NotEqual(t, old, renewed)

	sess, err := f.tokens.Verify(renewed)
	require.NoError(t, err)
	assert.Equal(t, u.ID, sess.UserID)
	assert.Equal(t, u.SessionEpoch, sess.Epoch)
	assert.WithinDuration(t, time.Now(), sess.IssuedAt, time.Minute,
		"the renewed token restarts the sliding window")
}

// A token that is still young must not be reissued: every renewal costs
// the BFF a Set-Cookie, and a cookie rewritten on every request would
// hide the real age of the session.
func TestRequireAuth_FreshTokenIsNotRenewed(t *testing.T) {
	f := newMiddlewareFixture(t)
	u := f.createUser(t, "+628123456789")

	token, _, err := f.tokens.Issue(u.ID, u.SessionEpoch)
	require.NoError(t, err)

	resp := f.ping(t, token)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Empty(t, resp.Header.Get(RenewHeader))
}

// Renewal must sit behind the epoch check, not beside it: an old token
// from a revoked epoch is exactly the one an attacker would replay, and
// handing it a fresh token would undo "log out everywhere".
func TestRequireAuth_RevokedEpochIsNotRenewed(t *testing.T) {
	f := newMiddlewareFixture(t)
	u := f.createUser(t, "+628123456789")

	stale, _, err := f.tokens.issueAt(u.ID, u.SessionEpoch-1, time.Now().Add(-8*24*time.Hour))
	require.NoError(t, err)

	resp := f.ping(t, stale)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Empty(t, resp.Header.Get(RenewHeader))
}
