package handler

import (
	"github.com/glennprays/catet-backend/internal/httperror"
	"github.com/glennprays/catet-backend/internal/service"
	"github.com/gofiber/fiber/v2"
)

// AnalyticsHandler serves the six /analytics/* metric endpoints.
type AnalyticsHandler struct {
	svc *service.AnalyticsService
}

func NewAnalyticsHandler(svc *service.AnalyticsService) *AnalyticsHandler {
	return &AnalyticsHandler{svc: svc}
}

func (h *AnalyticsHandler) CategoryBreakdown(c *fiber.Ctx) error {
	from, to, err := service.ResolveRange(qptr(c, "from"), qptr(c, "to"))
	if err != nil {
		return err
	}
	data, err := h.svc.CategoryBreakdown(c.UserContext(), from, to)
	if err != nil {
		return err
	}
	return httperror.OK(c, data)
}

func (h *AnalyticsHandler) IncomeExpense(c *fiber.Ctx) error {
	from, to, err := service.ResolveRange(qptr(c, "from"), qptr(c, "to"))
	if err != nil {
		return err
	}
	data, err := h.svc.IncomeExpense(c.UserContext(), from, to)
	if err != nil {
		return err
	}
	return httperror.OK(c, data)
}

func (h *AnalyticsHandler) StsTrend(c *fiber.Ctx) error {
	from, to, err := service.ResolveRange(qptr(c, "from"), qptr(c, "to"))
	if err != nil {
		return err
	}
	data, err := h.svc.StsTrend(c.UserContext(), from, to)
	if err != nil {
		return err
	}
	return httperror.OK(c, data)
}

func (h *AnalyticsHandler) Heatmap(c *fiber.Ctx) error {
	from, to, err := service.ResolveRange(qptr(c, "from"), qptr(c, "to"))
	if err != nil {
		return err
	}
	data, err := h.svc.Heatmap(c.UserContext(), from, to)
	if err != nil {
		return err
	}
	return httperror.OK(c, data)
}

func (h *AnalyticsHandler) TopMerchants(c *fiber.Ctx) error {
	from, to, err := service.ResolveRange(qptr(c, "from"), qptr(c, "to"))
	if err != nil {
		return err
	}
	data, err := h.svc.TopMerchants(c.UserContext(), from, to, c.QueryInt("limit", 10))
	if err != nil {
		return err
	}
	return httperror.OK(c, data)
}

func (h *AnalyticsHandler) EssentialLifestyle(c *fiber.Ctx) error {
	from, to, err := service.ResolveRange(qptr(c, "from"), qptr(c, "to"))
	if err != nil {
		return err
	}
	data, err := h.svc.EssentialLifestyle(c.UserContext(), from, to)
	if err != nil {
		return err
	}
	return httperror.OK(c, data)
}
