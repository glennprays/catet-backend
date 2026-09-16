package handler

import (
	"github.com/glennprays/catet-backend/internal/httperror"
	"github.com/glennprays/catet-backend/internal/params/request"
	"github.com/glennprays/catet-backend/internal/service"
	"github.com/gofiber/fiber/v2"
)

// AdminLLMHandler serves /admin/llm: the instance-wide AI provider.
// Admin-only gating lives on the route group, not here.
type AdminLLMHandler struct {
	svc *service.LLMProviderService
}

func NewAdminLLMHandler(svc *service.LLMProviderService) *AdminLLMHandler {
	return &AdminLLMHandler{svc: svc}
}

func (h *AdminLLMHandler) Get(c *fiber.Ctx) error {
	data, err := h.svc.Get(c.UserContext())
	if err != nil {
		return err
	}
	return httperror.OK(c, data)
}

func (h *AdminLLMHandler) Update(c *fiber.Ctx) error {
	req, err := httperror.BindAndValidate[request.UpdateLLMProviderRequest](c)
	if err != nil {
		return err
	}
	data, err := h.svc.Update(c.UserContext(), service.UpdateLLMProviderInput{
		BaseURL:              req.BaseURL,
		APIKey:               req.APIKey,
		Model:                req.Model,
		VisionModel:          req.VisionModel,
		TimeoutSeconds:       req.TimeoutSeconds,
		VisionTimeoutSeconds: req.VisionTimeoutSeconds,
	})
	if err != nil {
		return err
	}
	return httperror.OK(c, data)
}

// Reset drops the dashboard override; the answer is the provider the
// instance falls back to, so the dashboard can render it immediately.
func (h *AdminLLMHandler) Reset(c *fiber.Ctx) error {
	if err := h.svc.Reset(c.UserContext()); err != nil {
		return err
	}
	data, err := h.svc.Get(c.UserContext())
	if err != nil {
		return err
	}
	return httperror.OK(c, data)
}

// Models is a POST because it carries the draft base URL and key: the
// admin lists what a provider serves before saving anything, and a
// secret does not belong in a query string.
func (h *AdminLLMHandler) Models(c *fiber.Ctx) error {
	req, err := httperror.BindAndValidate[request.LLMModelsRequest](c)
	if err != nil {
		return err
	}
	data, err := h.svc.ListModels(c.UserContext(), req.BaseURL, req.APIKey)
	if err != nil {
		return err
	}
	return httperror.OK(c, data)
}

func (h *AdminLLMHandler) Test(c *fiber.Ctx) error {
	req, err := httperror.BindAndValidate[request.UpdateLLMProviderRequest](c)
	if err != nil {
		return err
	}
	data, err := h.svc.Test(c.UserContext(), service.UpdateLLMProviderInput{
		BaseURL:              req.BaseURL,
		APIKey:               req.APIKey,
		Model:                req.Model,
		VisionModel:          req.VisionModel,
		TimeoutSeconds:       req.TimeoutSeconds,
		VisionTimeoutSeconds: req.VisionTimeoutSeconds,
	})
	if err != nil {
		return err
	}
	return httperror.OK(c, data)
}
