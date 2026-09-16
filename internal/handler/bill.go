package handler

import (
	"github.com/glennprays/catet-backend/internal/apperror"
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
		Name:            req.Name,
		AmountMinMinor:  req.AmountMinMinor,
		AmountMaxMinor:  req.AmountMaxMinor,
		Cadence:         req.Cadence,
		NextDueDate:     req.NextDueDate,
		LinkedSourceID:  req.LinkedSourceID,
		CategoryID:      req.CategoryID,
		RemainingCount:  req.RemainingCount,
		ReminderOffsets: req.ReminderOffsets,
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
	in := service.UpdateBillInput{
		Name:            req.Name,
		AmountMinMinor:  req.AmountMinMinor,
		AmountMaxMinor:  req.AmountMaxMinor,
		Cadence:         req.Cadence,
		NextDueDate:     req.NextDueDate,
		LinkedSourceID:  req.LinkedSourceID,
		CategoryID:      req.CategoryID,
		ReminderOffsets: req.ReminderOffsets,
	}
	if req.RemainingCount.Set {
		in.RemainingCount = &service.NullableInt{Value: req.RemainingCount.Value}
	}
	bill, err := h.svc.Update(c.UserContext(), id, in)
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

func (h *BillHandler) Reactivate(c *fiber.Ctx) error {
	id, err := uuidParam(c, "id")
	if err != nil {
		return err
	}
	if err := h.svc.Reactivate(c.UserContext(), id); err != nil {
		return err
	}
	return httperror.OK(c, response.BillActiveData{Active: true})
}

// Pay settles one occurrence from the dashboard: a peer capture surface
// (ADR-0004), so the write is origin=dashboard_manual with no AI gate.
func (h *BillHandler) Pay(c *fiber.Ctx) error {
	id, err := uuidParam(c, "id")
	if err != nil {
		return err
	}
	req, err := httperror.BindAndValidate[request.PayBillRequest](c)
	if err != nil {
		return err
	}
	data, err := h.svc.Pay(c.UserContext(), id, service.PayBillInput{
		AmountMinor:  req.AmountMinor,
		SourceID:     req.SourceID,
		OccurredDate: req.OccurredDate,
		Origin:       "dashboard_manual",
	})
	if err != nil {
		return err
	}
	return httperror.OK(c, data)
}

func (h *BillHandler) Payments(c *fiber.Ctx) error {
	from := c.Query("from")
	to := c.Query("to")
	if from == "" || to == "" {
		return apperror.BadRequestf("from and to are required (YYYY-MM-DD)")
	}
	payments, err := h.svc.Payments(c.UserContext(), from, to)
	if err != nil {
		return err
	}
	return httperror.OK(c, response.BillPaymentsData{Payments: payments})
}
