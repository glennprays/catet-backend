package handler

import (
	"github.com/glennprays/catet-backend/internal/apperror"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
)

// uuidParam parses a path parameter as a UUID.
func uuidParam(c *fiber.Ctx, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(c.Params(name))
	if err != nil {
		return uuid.Nil, apperror.BadRequest("invalid id parameter")
	}
	return id, nil
}

// qptr returns a pointer to the query value, or nil when absent/empty.
func qptr(c *fiber.Ctx, name string) *string {
	v := c.Query(name)
	if v == "" {
		return nil
	}
	return &v
}
