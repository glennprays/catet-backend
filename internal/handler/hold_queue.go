package handler

import (
	"github.com/glennprays/catet-backend/internal/httperror"
	"github.com/glennprays/catet-backend/internal/params/request"
	"github.com/glennprays/catet-backend/internal/params/response"
	"github.com/glennprays/catet-backend/internal/service"
	"github.com/gofiber/fiber/v2"
)

// HoldQueueHandler serves /hold-queue and its resolve endpoint.
type HoldQueueHandler struct {
	svc *service.HoldQueueService
}

func NewHoldQueueHandler(svc *service.HoldQueueService) *HoldQueueHandler {
	return &HoldQueueHandler{svc: svc}
}

func (h *HoldQueueHandler) List(c *fiber.Ctx) error {
	items, err := h.svc.List(c.UserContext())
	if err != nil {
		return err
	}
	return httperror.OK(c, response.HoldQueueData{Items: items})
}

func (h *HoldQueueHandler) Resolve(c *fiber.Ctx) error {
	id, err := uuidParam(c, "id")
	if err != nil {
		return err
	}
	req, err := httperror.BindAndValidate[request.ResolveHoldRequest](c)
	if err != nil {
		return err
	}
	if err := h.svc.Resolve(c.UserContext(), id, req.Action); err != nil {
		return err
	}
	return httperror.OK(c, response.ResolvedData{Resolved: true})
}
