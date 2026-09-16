package handler

import (
	"github.com/glennprays/catet-backend/internal/httperror"
	"github.com/glennprays/catet-backend/internal/params/request"
	"github.com/glennprays/catet-backend/internal/params/response"
	"github.com/glennprays/catet-backend/internal/service"
	"github.com/gofiber/fiber/v2"
)

// SourceHandler serves the /sources endpoints.
type SourceHandler struct {
	svc *service.SourceService
}

func NewSourceHandler(svc *service.SourceService) *SourceHandler {
	return &SourceHandler{svc: svc}
}

func (h *SourceHandler) List(c *fiber.Ctx) error {
	includeArchived := c.Query("include_archived") == "true" || c.Query("include_archived") == "1"
	data, err := h.svc.List(c.UserContext(), includeArchived)
	if err != nil {
		return err
	}
	return httperror.OK(c, data)
}

func (h *SourceHandler) Create(c *fiber.Ctx) error {
	req, err := httperror.BindAndValidate[request.CreateSourceRequest](c)
	if err != nil {
		return err
	}
	out, err := h.svc.Create(c.UserContext(), service.CreateSourceInput{
		Name:     req.Name,
		Type:     req.Type,
		IsDebt:   req.IsDebt,
		Currency: req.Currency,
	})
	if err != nil {
		return err
	}
	return httperror.OK(c, out)
}

func (h *SourceHandler) Update(c *fiber.Ctx) error {
	id, err := uuidParam(c, "id")
	if err != nil {
		return err
	}
	req, err := httperror.BindAndValidate[request.UpdateSourceRequest](c)
	if err != nil {
		return err
	}
	out, err := h.svc.Update(c.UserContext(), id, service.UpdateSourceInput{Name: req.Name, Type: req.Type, IsDebt: req.IsDebt})
	if err != nil {
		return err
	}
	return httperror.OK(c, out)
}

func (h *SourceHandler) Archive(c *fiber.Ctx) error {
	id, err := uuidParam(c, "id")
	if err != nil {
		return err
	}
	if err := h.svc.Archive(c.UserContext(), id); err != nil {
		return err
	}
	return httperror.OK(c, response.ArchivedData{Archived: true})
}

func (h *SourceHandler) Restore(c *fiber.Ctx) error {
	id, err := uuidParam(c, "id")
	if err != nil {
		return err
	}
	out, err := h.svc.Restore(c.UserContext(), id)
	if err != nil {
		return err
	}
	return httperror.OK(c, out)
}

func (h *SourceHandler) DeletePermanently(c *fiber.Ctx) error {
	id, err := uuidParam(c, "id")
	if err != nil {
		return err
	}
	if err := h.svc.DeletePermanently(c.UserContext(), id); err != nil {
		return err
	}
	return httperror.OK(c, response.DeletedData{Deleted: true})
}

func (h *SourceHandler) Adjust(c *fiber.Ctx) error {
	id, err := uuidParam(c, "id")
	if err != nil {
		return err
	}
	req, err := httperror.BindAndValidate[request.AdjustSourceRequest](c)
	if err != nil {
		return err
	}
	tx, err := h.svc.Adjust(c.UserContext(), id, req.AmountMinor, req.Note)
	if err != nil {
		return err
	}
	return httperror.OK(c, tx)
}
