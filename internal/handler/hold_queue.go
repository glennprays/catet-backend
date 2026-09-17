package handler

import (
	"github.com/glennprays/catet-backend/internal/httperror"
	"github.com/glennprays/catet-backend/internal/params/request"
	"github.com/glennprays/catet-backend/internal/params/response"
	"github.com/glennprays/catet-backend/internal/service"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
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
	var overrides *service.PendingOverrides
	if req.Overrides != nil {
		overrides = &service.PendingOverrides{
			AmountMinor:         req.Overrides.AmountMinor,
			SourceID:            req.Overrides.SourceID,
			DestinationSourceID: req.Overrides.DestinationSourceID,
			CategoryID:          req.Overrides.CategoryID,
			Merchant:            req.Overrides.Merchant,
			Note:                req.Overrides.Note,
			OccurredDate:        req.Overrides.OccurredDate,
			Collapse:            req.Overrides.Collapse,
		}
		if len(req.Overrides.Items) > 0 {
			items := make([]service.PendingItemOverride, 0, len(req.Overrides.Items))
			for _, it := range req.Overrides.Items {
				items = append(items, service.PendingItemOverride{N: it.N, CategoryID: it.CategoryID})
			}
			overrides.Items = items
		}
	}
	txs, err := h.svc.Resolve(c.UserContext(), id, req.Action, overrides)
	if err != nil {
		return err
	}
	out := response.ResolvedData{Resolved: true}
	if len(txs) > 0 {
		out.TransactionID = &txs[0].ID
		out.TransactionIDs = make([]uuid.UUID, 0, len(txs))
		for _, tx := range txs {
			out.TransactionIDs = append(out.TransactionIDs, tx.ID)
		}
	}
	return httperror.OK(c, out)
}
