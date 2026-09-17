package handler

import (
	"github.com/glennprays/catet-backend/internal/apperror"
	"github.com/glennprays/catet-backend/internal/httperror"
	"github.com/glennprays/catet-backend/internal/params/request"
	"github.com/glennprays/catet-backend/internal/params/response"
	"github.com/glennprays/catet-backend/internal/service"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
)

// TransactionHandler serves the /transactions endpoints.
type TransactionHandler struct {
	svc *service.TransactionService
}

func NewTransactionHandler(svc *service.TransactionService) *TransactionHandler {
	return &TransactionHandler{svc: svc}
}

func (h *TransactionHandler) List(c *fiber.Ctx) error {
	in := service.ListInput{Limit: c.QueryInt("limit", 0)}
	parseID := func(name string) (*uuid.UUID, error) {
		v := c.Query(name)
		if v == "" {
			return nil, nil
		}
		id, err := uuid.Parse(v)
		if err != nil {
			return nil, apperror.Validation("invalid query parameter").
				WithDetails(apperror.FieldError{Field: name, Rule: "uuid", Message: "must be a UUID"})
		}
		return &id, nil
	}
	sourceID, err := parseID("source_id")
	if err != nil {
		return err
	}
	categoryID, err := parseID("category_id")
	if err != nil {
		return err
	}
	merchantID, err := parseID("merchant_id")
	if err != nil {
		return err
	}
	in.SourceID = sourceID
	in.CategoryID = categoryID
	in.MerchantID = merchantID
	in.ReviewStatus = qptr(c, "review_status")
	in.Query = qptr(c, "q")
	in.FromDate = qptr(c, "from")
	in.ToDate = qptr(c, "to")
	in.Direction = qptr(c, "direction")
	in.Voided = qptr(c, "voided")
	in.Cursor = qptr(c, "cursor")

	page, err := h.svc.List(c.UserContext(), in)
	if err != nil {
		return err
	}

	items := make([]response.LedgerItem, 0, len(page.Rows))
	for _, row := range page.Rows {
		items = append(items, service.LedgerItemFromRow(row))
	}
	out := httperror.Page{PageSize: len(items), Total: page.Total}
	if page.NextCursor != nil {
		out.NextCursor = *page.NextCursor
	}
	return httperror.OKPaginated(c, response.TransactionListData{Transactions: items}, out)
}

func (h *TransactionHandler) Create(c *fiber.Ctx) error {
	req, err := httperror.BindAndValidate[request.CreateTransactionRequest](c)
	if err != nil {
		return err
	}
	tx, err := h.svc.Create(c.UserContext(), service.CreateInput{
		Type:                req.Type,
		OccurredAt:          req.OccurredAt,
		OccurredDate:        req.OccurredDate,
		AmountMinor:         req.AmountMinor,
		SourceID:            req.SourceID,
		DestinationSourceID: req.DestinationSourceID,
		CategoryID:          req.CategoryID,
		Note:                req.Note,
		Merchant:            req.Merchant,
		Origin:              "dashboard_manual",
	})
	if err != nil {
		return err
	}
	return httperror.OK(c, tx)
}

func (h *TransactionHandler) Get(c *fiber.Ctx) error {
	id, err := uuidParam(c, "id")
	if err != nil {
		return err
	}
	tx, err := h.svc.Get(c.UserContext(), id)
	if err != nil {
		return err
	}
	return httperror.OK(c, tx)
}

func (h *TransactionHandler) Update(c *fiber.Ctx) error {
	id, err := uuidParam(c, "id")
	if err != nil {
		return err
	}
	req, err := httperror.BindAndValidate[request.UpdateTransactionRequest](c)
	if err != nil {
		return err
	}
	tx, err := h.svc.Update(c.UserContext(), id, service.UpdateInput{
		AmountMinor: req.AmountMinor,
		OccurredAt:  req.OccurredAt,
		CategoryID:  req.CategoryID,
		Note:        req.Note,
		Merchant:    req.Merchant,
	})
	if err != nil {
		return err
	}
	return httperror.OK(c, tx)
}

func (h *TransactionHandler) Void(c *fiber.Ctx) error {
	id, err := uuidParam(c, "id")
	if err != nil {
		return err
	}
	if err := h.svc.Void(c.UserContext(), id); err != nil {
		return err
	}
	return httperror.OK(c, response.VoidedData{Voided: true})
}

func (h *TransactionHandler) Reverse(c *fiber.Ctx) error {
	id, err := uuidParam(c, "id")
	if err != nil {
		return err
	}
	var req request.ReverseTransactionRequest
	if len(c.Body()) > 0 {
		if err := c.BodyParser(&req); err != nil {
			return apperror.BadRequestf("invalid request body: %v", err)
		}
	}
	tx, err := h.svc.Reverse(c.UserContext(), id, req.Note, "dashboard_manual")
	if err != nil {
		return err
	}
	return httperror.OK(c, tx)
}
