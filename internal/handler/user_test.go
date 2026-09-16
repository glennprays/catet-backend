package handler_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/glennprays/catet-backend/internal/auth"
	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/gateway"
	"github.com/glennprays/catet-backend/internal/handler"
	"github.com/glennprays/catet-backend/internal/params/response"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/glennprays/catet-backend/internal/testkit"
	"github.com/glennprays/log"
	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type usersFixture struct {
	app    *fiber.App
	users  *repository.UserRepository
	tokens *auth.TokenService
	pool   *pgxpool.Pool
}

// newUsersFixture mounts the admin users route the way internal/router
// does: RequireAuth on the protected group, RequireAdmin on the
// /users group. The gate is the point of these tests, so it has to be
// the real chain and not a hand-rolled stand-in.
func newUsersFixture(t *testing.T) *usersFixture {
	t.Helper()
	pool := testkit.NewDB(t)

	logger, err := log.New(log.Config{
		Service: "test",
		Env:     "development",
		Level:   log.Level("error"),
		Output:  log.OutputType("stdout"),
	})
	require.NoError(t, err, "create test logger")

	users := repository.NewUserRepository(pool)
	tokens := auth.NewTokenService("test-secret")
	invites := auth.NewInviteService(
		repository.NewInviteRepository(pool), users,
		gateway.NewLogSender(logger), logger, "http://localhost:3001",
	)
	h := handler.NewUserHandler(users, invites)

	app := testkit.NewTestApp(t, func(app *fiber.App) {
		v1 := app.Group("/api/v1")
		protected := v1.Group("", auth.RequireAuth(tokens, users))
		admin := protected.Group("/users", auth.RequireAdmin)
		admin.Get("", h.List)
	})
	return &usersFixture{app: app, users: users, tokens: tokens, pool: pool}
}

func (f *usersFixture) createUser(t *testing.T, phone, name string, isAdmin bool) *entity.User {
	t.Helper()
	u, err := f.users.Create(t.Context(), &entity.User{
		Phone: phone, DisplayName: name, IsAdmin: isAdmin,
	})
	require.NoError(t, err)
	return u
}

func (f *usersFixture) session(t *testing.T, u *entity.User) string {
	t.Helper()
	token, _, err := f.tokens.Issue(u.ID, u.SessionEpoch)
	require.NoError(t, err)
	return token
}

// listUsers performs GET /api/v1/users with the given bearer token.
func (f *usersFixture) listUsers(t *testing.T, token string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/users", nil)
	req.Header.Set(fiber.HeaderAuthorization, "Bearer "+token)
	resp, err := f.app.Test(req)
	require.NoError(t, err)
	return resp
}

func decodeEnvelope[T any](t *testing.T, resp *http.Response) (T, string) {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var env struct {
		Data  T `json:"data"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(body, &env), "response must be the standard envelope: %s", body)
	if env.Error != nil {
		return env.Data, env.Error.Code
	}
	return env.Data, ""
}

// Account management is the one surface where one user acts on another,
// so a normal session must not reach it — the whole point of the
// per-user ledger is that nobody else can enumerate it.
func TestUserHandler_NonAdminIsForbidden(t *testing.T) {
	f := newUsersFixture(t)
	f.createUser(t, "+628123456789", "Owner", true)
	member := f.createUser(t, "+628999999999", "Budi", false)

	resp := f.listUsers(t, f.session(t, member))
	defer resp.Body.Close()

	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	_, code := decodeEnvelope[response.UsersData](t, resp)
	assert.Equal(t, "FORBIDDEN", code)
}

func TestUserHandler_AdminListsEveryAccount(t *testing.T) {
	f := newUsersFixture(t)
	admin := f.createUser(t, "+628123456789", "Owner", true)
	f.createUser(t, "+628999999999", "Budi", false)

	resp := f.listUsers(t, f.session(t, admin))
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	data, code := decodeEnvelope[response.UsersData](t, resp)
	require.Empty(t, code)
	require.Len(t, data.Users, 2)
	assert.Equal(t, "+628123456789", data.Users[0].Phone, "ordered by creation")
	assert.Equal(t, "Budi", data.Users[1].DisplayName)
	assert.True(t, data.Users[0].IsAdmin)
	assert.Nil(t, data.Users[1].DeactivatedAt)
}

// Deactivation has to bite on the next request, not at the next login:
// the token is stateless, so RequireAuth re-reading the account is the
// only thing standing between a removed user and their old session.
func TestUserHandler_DeactivationKillsAnIssuedSession(t *testing.T) {
	f := newUsersFixture(t)
	admin := f.createUser(t, "+628123456789", "Owner", true)
	token := f.session(t, admin)

	resp := f.listUsers(t, token)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, resp.Body.Close())

	now := time.Now().UTC()
	require.NoError(t, f.users.SetDeactivated(t.Context(), admin.ID, &now))

	resp = f.listUsers(t, token)
	defer resp.Body.Close()

	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	_, code := decodeEnvelope[response.UsersData](t, resp)
	assert.Equal(t, "UNAUTHORIZED", code)
}

// The same session dies when the epoch moves, which is what a password
// change and "log out everywhere" both do.
func TestUserHandler_EpochBumpKillsAnIssuedSession(t *testing.T) {
	f := newUsersFixture(t)
	admin := f.createUser(t, "+628123456789", "Owner", true)
	token := f.session(t, admin)

	require.NoError(t, f.users.BumpEpoch(t.Context(), admin.ID))

	resp := f.listUsers(t, token)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}
