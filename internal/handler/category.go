package handler

import (
	"encoding/json"

	"github.com/glennprays/catet-backend/internal/httperror"
	"github.com/glennprays/catet-backend/internal/params/request"
	"github.com/glennprays/catet-backend/internal/params/response"
	"github.com/glennprays/catet-backend/internal/service"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
)

// CategoryHandler serves the /categories endpoints.
type CategoryHandler struct {
	svc *service.CategoryService
}

func NewCategoryHandler(svc *service.CategoryService) *CategoryHandler {
	return &CategoryHandler{svc: svc}
}

func (h *CategoryHandler) List(c *fiber.Ctx) error {
	includeArchived := c.Query("include_archived") == "true" || c.Query("include_archived") == "1"
	cats, err := h.svc.List(c.UserContext(), includeArchived)
	if err != nil {
		return err
	}
	return httperror.OK(c, response.CategoriesData{Categories: cats})
}

func (h *CategoryHandler) Create(c *fiber.Ctx) error {
	req, err := httperror.BindAndValidate[request.CreateCategoryRequest](c)
	if err != nil {
		return err
	}
	cat, err := h.svc.Create(c.UserContext(), service.CreateCategoryInput{
		Name:      req.Name,
		ParentID:  req.ParentID,
		Icon:      req.Icon,
		Essential: req.Essential,
	})
	if err != nil {
		return err
	}
	return httperror.OK(c, cat)
}

func (h *CategoryHandler) Update(c *fiber.Ctx) error {
	id, err := uuidParam(c, "id")
	if err != nil {
		return err
	}
	req, err := httperror.BindAndValidate[request.UpdateCategoryRequest](c)
	if err != nil {
		return err
	}
	in := service.UpdateCategoryInput{
		Name:      req.Name,
		Icon:      req.Icon,
		Essential: req.Essential,
	}
	if req.ParentID != nil {
		in.ParentID = &req.ParentID
	} else if parentFieldPresent(c.Body()) {
		// "parent_id": null — present but null clears the parent.
		in.ParentID = new(*uuid.UUID)
	}
	cat, err := h.svc.Update(c.UserContext(), id, in)
	if err != nil {
		return err
	}
	return httperror.OK(c, cat)
}

// parentFieldPresent reports whether the raw JSON body carries a
// parent_id key (encoding/json leaves *uuid.UUID nil for both absent
// and null, so presence needs a raw check).
func parentFieldPresent(body []byte) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil {
		return false
	}
	_, ok := fields["parent_id"]
	return ok
}

func (h *CategoryHandler) Archive(c *fiber.Ctx) error {
	id, err := uuidParam(c, "id")
	if err != nil {
		return err
	}
	if err := h.svc.Archive(c.UserContext(), id); err != nil {
		return err
	}
	return httperror.OK(c, response.ArchivedData{Archived: true})
}
