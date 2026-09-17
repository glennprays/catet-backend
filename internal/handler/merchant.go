package handler

import (
	"github.com/glennprays/catet-backend/internal/httperror"
	"github.com/glennprays/catet-backend/internal/params/request"
	"github.com/glennprays/catet-backend/internal/params/response"
	"github.com/glennprays/catet-backend/internal/service"
	"github.com/gofiber/fiber/v2"
)

// MerchantHandler serves the /merchants endpoints: the Settings screen
// where the user renames and merges what the capture surfaces named.
type MerchantHandler struct {
	svc *service.MerchantService
}

func NewMerchantHandler(svc *service.MerchantService) *MerchantHandler {
	return &MerchantHandler{svc: svc}
}

func (h *MerchantHandler) List(c *fiber.Ctx) error {
	rows, err := h.svc.List(c.UserContext())
	if err != nil {
		return err
	}
	items := make([]response.MerchantSummary, 0, len(rows))
	for _, row := range rows {
		items = append(items, response.MerchantSummary{
			ID:         row.ID,
			Name:       row.Name,
			TxCount:    row.TxCount,
			TotalMinor: row.TotalMinor,
			LastUsedAt: row.LastUsedAt,
		})
	}
	return httperror.OK(c, response.MerchantListData{Merchants: items})
}

func (h *MerchantHandler) Rename(c *fiber.Ctx) error {
	id, err := uuidParam(c, "id")
	if err != nil {
		return err
	}
	req, err := httperror.BindAndValidate[request.RenameMerchantRequest](c)
	if err != nil {
		return err
	}
	m, err := h.svc.Rename(c.UserContext(), id, req.Name)
	if err != nil {
		return err
	}
	return httperror.OK(c, response.MerchantData{Merchant: *m})
}

func (h *MerchantHandler) Merge(c *fiber.Ctx) error {
	id, err := uuidParam(c, "id")
	if err != nil {
		return err
	}
	req, err := httperror.BindAndValidate[request.MergeMerchantRequest](c)
	if err != nil {
		return err
	}
	if err := h.svc.Merge(c.UserContext(), id, req.IntoMerchantID); err != nil {
		return err
	}
	return httperror.OK(c, response.MergedMerchantData{Merged: true, IntoMerchantID: req.IntoMerchantID})
}
