package handler

import (
	"github.com/glennprays/catet-backend/internal/httperror"
	"github.com/glennprays/catet-backend/internal/params/request"
	"github.com/glennprays/catet-backend/internal/params/response"
	"github.com/glennprays/catet-backend/internal/service"
	"github.com/gofiber/fiber/v2"
)

// BudgetHandler serves the /budgets endpoints.
type BudgetHandler struct {
	svc *service.BudgetService
}

func NewBudgetHandler(svc *service.BudgetService) *BudgetHandler {
	return &BudgetHandler{svc: svc}
}

func (h *BudgetHandler) List(c *fiber.Ctx) error {
	period, rows, err := h.svc.List(c.UserContext(), qptr(c, "period"))
	if err != nil {
		return err
	}
	return httperror.OK(c, response.BudgetsData{Period: period, Budgets: rows})
}

func (h *BudgetHandler) Create(c *fiber.Ctx) error {
	req, err := httperror.BindAndValidate[request.CreateBudgetRequest](c)
	if err != nil {
		return err
	}
	recurring := true
	if req.Recurring != nil {
		recurring = *req.Recurring
	}
	row, err := h.svc.Create(c.UserContext(), req.CategoryID, req.Period, req.AmountMinor, recurring)
	if err != nil {
		return err
	}
	return httperror.OK(c, row)
}

func (h *BudgetHandler) Update(c *fiber.Ctx) error {
	id, err := uuidParam(c, "id")
	if err != nil {
		return err
	}
	req, err := httperror.BindAndValidate[request.UpdateBudgetRequest](c)
	if err != nil {
		return err
	}
	row, err := h.svc.UpdateAmount(c.UserContext(), id, req.Period, req.Scope, req.AmountMinor)
	if err != nil {
		return err
	}
	return httperror.OK(c, row)
}

// Stop ends the rule from the given period on; earlier months stay.
func (h *BudgetHandler) Stop(c *fiber.Ctx) error {
	id, err := uuidParam(c, "id")
	if err != nil {
		return err
	}
	req, err := httperror.BindAndValidate[request.StopBudgetRequest](c)
	if err != nil {
		return err
	}
	if err := h.svc.Stop(c.UserContext(), id, req.Period); err != nil {
		return err
	}
	return httperror.OK(c, response.DeletedData{Deleted: true})
}

// Delete drops the rule and its whole history.
func (h *BudgetHandler) Delete(c *fiber.Ctx) error {
	id, err := uuidParam(c, "id")
	if err != nil {
		return err
	}
	if err := h.svc.DeleteAll(c.UserContext(), id); err != nil {
		return err
	}
	return httperror.OK(c, response.DeletedData{Deleted: true})
}
