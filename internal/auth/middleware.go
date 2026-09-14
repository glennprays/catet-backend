package auth

import (
	"strings"

	"github.com/glennprays/catet-backend/internal/apperror"
	"github.com/gofiber/fiber/v2"
)

const phoneLocalsKey = "auth_phone"

// RequireAuth validates the Authorization: Bearer JWT and stores the
// subject phone in Locals for handlers (auth.Phone).
func RequireAuth(tokens *TokenService) fiber.Handler {
	return func(c *fiber.Ctx) error {
		header := c.Get(fiber.HeaderAuthorization)
		const prefix = "Bearer "
		if !strings.HasPrefix(header, prefix) {
			return apperror.Unauthorized("missing bearer token")
		}
		phone, err := tokens.Verify(strings.TrimPrefix(header, prefix))
		if err != nil {
			return err
		}
		c.Locals(phoneLocalsKey, phone)
		return c.Next()
	}
}

// Phone returns the authenticated subject phone set by RequireAuth.
func Phone(c *fiber.Ctx) string {
	if phone, ok := c.Locals(phoneLocalsKey).(string); ok {
		return phone
	}
	return ""
}
