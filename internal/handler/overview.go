package handler

import (
	"github.com/glennprays/catet-backend/internal/httperror"
	"github.com/glennprays/catet-backend/internal/service"
	"github.com/gofiber/fiber/v2"
)

// OverviewHandler serves /overview and /badges.
type OverviewHandler struct {
	overview *service.OverviewService
	badges   *service.BadgesService
}

func NewOverviewHandler(overview *service.OverviewService, badges *service.BadgesService) *OverviewHandler {
	return &OverviewHandler{overview: overview, badges: badges}
}

func (h *OverviewHandler) Get(c *fiber.Ctx) error {
	data, err := h.overview.Build(c.UserContext())
	if err != nil {
		return err
	}
	return httperror.OK(c, data)
}

func (h *OverviewHandler) Badges(c *fiber.Ctx) error {
	counts, err := h.badges.Get(c.UserContext())
	if err != nil {
		return err
	}
	return httperror.OK(c, counts)
}
