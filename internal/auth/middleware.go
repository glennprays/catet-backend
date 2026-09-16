package auth

import (
	"strings"
	"time"

	"github.com/glennprays/catet-backend/internal/apperror"
	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/glennprays/catet-backend/internal/scope"
	"github.com/gofiber/fiber/v2"
)

const userLocalsKey = "auth_user"

// RequireAuth validates the Authorization: Bearer JWT, loads the
// account it names, and scopes the request context to it.
//
// The account is re-read on every request, not trusted from the token.
// That one query is what makes revocation work without a session store:
// a deactivated account and a bumped session_epoch (password change,
// reset, "log out everywhere") both take effect on the next request,
// and every failure collapses to the same 401 so a valid-looking token
// cannot be used to probe account state.
//
// A token older than RenewAfter is answered with a fresh one in
// RenewHeader: the dashboard BFF re-sets its cookie, so an active
// device never walks into the 30-day expiry.
func RequireAuth(tokens *TokenService, users *repository.UserRepository) fiber.Handler {
	return func(c *fiber.Ctx) error {
		header := c.Get(fiber.HeaderAuthorization)
		const prefix = "Bearer "
		if !strings.HasPrefix(header, prefix) {
			return apperror.Unauthorized("missing bearer token")
		}
		sess, err := tokens.Verify(strings.TrimPrefix(header, prefix))
		if err != nil {
			return err
		}
		user, err := users.GetByID(c.UserContext(), sess.UserID)
		if err != nil {
			return apperror.Internal("failed to load session user").Wrap(err)
		}
		if user == nil || user.DeactivatedAt != nil || user.SessionEpoch != sess.Epoch {
			return apperror.Unauthorized("invalid or expired session")
		}
		if time.Since(sess.IssuedAt) >= RenewAfter {
			// A mint failure is not the caller's problem: the token they
			// presented is still valid, they just do not get a new one.
			if renewed, _, err := tokens.Issue(user.ID, user.SessionEpoch); err == nil {
				c.Set(RenewHeader, renewed)
			}
		}

		c.Locals(userLocalsKey, user)
		c.SetUserContext(scope.WithUserID(c.UserContext(), user.ID))
		return c.Next()
	}
}

// RequireAdmin gates the account-management routes. Admin is the
// bootstrap account and nothing else — there is no promotion path — so
// this is a flag check, not a role system.
func RequireAdmin(c *fiber.Ctx) error {
	user := CurrentUser(c)
	if user == nil || !user.IsAdmin {
		return apperror.Forbidden("admin only")
	}
	return c.Next()
}

// CurrentUser returns the account RequireAuth authenticated, or nil on
// an unprotected route.
func CurrentUser(c *fiber.Ctx) *entity.User {
	if user, ok := c.Locals(userLocalsKey).(*entity.User); ok {
		return user
	}
	return nil
}
