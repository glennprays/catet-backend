package handler

import (
	"github.com/glennprays/catet-backend/internal/httperror"
	"github.com/glennprays/catet-backend/internal/params/request"
	"github.com/glennprays/catet-backend/internal/params/response"
	"github.com/glennprays/catet-backend/internal/service"
	"github.com/gofiber/fiber/v2"
)

// BillHandler serves the /bills endpoints.
type BillHandler struct {
	svc *service.BillService
}

func NewBillHandler(svc *service.BillService) *BillHandler {
	return &BillHandler{svc: svc}
}

func (h *BillHandler) List(c *fiber.Ctx) error {
	bills, err := h.svc.List(c.UserContext())
	if err != nil {
		return err
	}
	return httperror.OK(c, response.BillsData{Bills: bills})
}

func (h *BillHandler) Create(c *fiber.Ctx) error {
	req, err := httperror.BindAndValidate[request.CreateBillRequest](c)
	if err != nil {
		return err
	}
	bill, err := h.svc.Create(c.UserContext(), service.CreateBillInput{
		Name:           req.Name,
		AmountMinMinor: req.AmountMinMinor,
		AmountMaxMinor: req.AmountMaxMinor,
		Cadence:        req.Cadence,
		NextDueDate:    req.NextDueDate,
		LinkedSourceID: req.LinkedSourceID,
		CategoryID:     req.CategoryID,
	})
	if err != nil {
		return err
	}
	return httperror.OK(c, bill)
}

func (h *BillHandler) Update(c *fiber.Ctx) error {
	id, err := uuidParam(c, "id")
	if err != nil {
		return err
	}
	req, err := httperror.BindAndValidate[request.UpdateBillRequest](c)
	if err != nil {
		return err
	}
	bill, err := h.svc.Update(c.UserContext(), id, service.UpdateBillInput{
		Name:           req.Name,
		AmountMinMinor: req.AmountMinMinor,
		AmountMaxMinor: req.AmountMaxMinor,
		Cadence:        req.Cadence,
		NextDueDate:    req.NextDueDate,
		LinkedSourceID: req.LinkedSourceID,
		CategoryID:     req.CategoryID,
	})
	if err != nil {
		return err
	}
	return httperror.OK(c, bill)
}

func (h *BillHandler) Deactivate(c *fiber.Ctx) error {
	id, err := uuidParam(c, "id")
	if err != nil {
		return err
	}
	if err := h.svc.Deactivate(c.UserContext(), id); err != nil {
		return err
	}
	return httperror.OK(c, response.BillActiveData{Active: false})
}
