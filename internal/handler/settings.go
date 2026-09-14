package handler

import (
	"github.com/glennprays/catet-backend/internal/httperror"
	"github.com/glennprays/catet-backend/internal/params/request"
	"github.com/glennprays/catet-backend/internal/service"
	"github.com/gofiber/fiber/v2"
)

// SettingsHandler serves /settings.
type SettingsHandler struct {
	svc *service.SettingsService
}

func NewSettingsHandler(svc *service.SettingsService) *SettingsHandler {
	return &SettingsHandler{svc: svc}
}

func (h *SettingsHandler) Get(c *fiber.Ctx) error {
	data, err := h.svc.Get(c.UserContext())
	if err != nil {
		return err
	}
	return httperror.OK(c, data)
}

func (h *SettingsHandler) Update(c *fiber.Ctx) error {
	req, err := httperror.BindAndValidate[request.UpdateSettingsRequest](c)
	if err != nil {
		return err
	}
	data, err := h.svc.Update(c.UserContext(), req.IncomeHorizonDays)
	if err != nil {
		return err
	}
	return httperror.OK(c, data)
}
