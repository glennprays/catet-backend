package service

import (
	"context"

	"github.com/glennprays/catet-backend/internal/apperror"
	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/google/uuid"
)

// BillService handles the Bill entity feeding Safe-to-Spend
// reservations and the due calendar.
type BillService struct {
	repo *repository.BillRepository
}

func NewBillService(repo *repository.BillRepository) *BillService {
	return &BillService{repo: repo}
}

func (s *BillService) List(ctx context.Context) ([]entity.Bill, error) {
	bills, err := s.repo.List(ctx)
	if err != nil {
		return nil, apperror.Internal("failed to list bills").Wrap(err)
	}
	return bills, nil
}

// CreateInput is the parsed POST /bills body (dates as YYYY-MM-DD).
type CreateBillInput struct {
	Name           string
	AmountMinMinor int64
	AmountMaxMinor int64
	Cadence        string
	NextDueDate    string
	LinkedSourceID *uuid.UUID
	CategoryID     *uuid.UUID
}

func (s *BillService) Create(ctx context.Context, in CreateBillInput) (*entity.Bill, error) {
	due, err := parseJakartaDate(in.NextDueDate)
	if err != nil {
		return nil, apperror.Validation("bill validation failed").
			WithDetails(apperror.FieldError{Field: "next_due_date", Rule: "yyyy-mm-date", Message: "must be YYYY-MM-DD"})
	}
	if in.AmountMaxMinor < in.AmountMinMinor {
		return nil, apperror.Validation("bill validation failed").
			WithDetails(apperror.FieldError{Field: "amount_max_minor", Rule: "gtefield", Message: "band max must be ≥ band min"})
	}
	created, err := s.repo.Create(ctx, &entity.Bill{
		ID:             uuid.New(),
		Name:           in.Name,
		AmountMinMinor: in.AmountMinMinor,
		AmountMaxMinor: in.AmountMaxMinor,
		Cadence:        in.Cadence,
		NextDueDate:    due,
		LinkedSourceID: in.LinkedSourceID,
		CategoryID:     in.CategoryID,
		Active:         true,
	})
	if err != nil {
		return nil, apperror.Internal("failed to create bill").Wrap(err)
	}
	return created, nil
}

// UpdateInput is the parsed PATCH /bills/{id} body; pointer = present.
type UpdateBillInput struct {
	Name           *string
	AmountMinMinor *int64
	AmountMaxMinor *int64
	Cadence        *string
	NextDueDate    *string
	LinkedSourceID *uuid.UUID
	CategoryID     *uuid.UUID
}

func (s *BillService) Update(ctx context.Context, id uuid.UUID, in UpdateBillInput) (*entity.Bill, error) {
	existing, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, apperror.Internal("failed to load bill").Wrap(err)
	}
	if existing == nil {
		return nil, apperror.NotFound("bill not found")
	}

	fields := map[string]any{}
	if in.Name != nil {
		fields["name"] = *in.Name
	}
	if in.AmountMinMinor != nil {
		fields["amount_min_minor"] = *in.AmountMinMinor
	}
	if in.AmountMaxMinor != nil {
		fields["amount_max_minor"] = *in.AmountMaxMinor
	}
	if in.Cadence != nil {
		fields["cadence"] = *in.Cadence
	}
	if in.NextDueDate != nil && *in.NextDueDate != "" {
		due, err := parseJakartaDate(*in.NextDueDate)
		if err != nil {
			return nil, apperror.Validation("bill validation failed").
				WithDetails(apperror.FieldError{Field: "next_due_date", Rule: "yyyy-mm-date", Message: "must be YYYY-MM-DD"})
		}
		fields["next_due_date"] = due
	}
	if in.LinkedSourceID != nil {
		fields["linked_source_id"] = *in.LinkedSourceID
	}
	if in.CategoryID != nil {
		fields["category_id"] = *in.CategoryID
	}

	// Band sanity after the merge of old and new values.
	minor := existing.AmountMinMinor
	maxor := existing.AmountMaxMinor
	if v, ok := fields["amount_min_minor"]; ok {
		minor = v.(int64)
	}
	if v, ok := fields["amount_max_minor"]; ok {
		maxor = v.(int64)
	}
	if maxor < minor {
		return nil, apperror.Validation("bill validation failed").
			WithDetails(apperror.FieldError{Field: "amount_max_minor", Rule: "gtefield", Message: "band max must be ≥ band min"})
	}

	updated, err := s.repo.Update(ctx, id, fields)
	if err != nil {
		return nil, apperror.Internal("failed to update bill").Wrap(err)
	}
	return updated, nil
}

// Deactivate soft-disables a bill (kept for history and reporting).
func (s *BillService) Deactivate(ctx context.Context, id uuid.UUID) error {
	existing, err := s.repo.Get(ctx, id)
	if err != nil {
		return apperror.Internal("failed to load bill").Wrap(err)
	}
	if existing == nil {
		return apperror.NotFound("bill not found")
	}
	ok, err := s.repo.SetActive(ctx, id, false)
	if err != nil {
		return apperror.Internal("failed to deactivate bill").Wrap(err)
	}
	if !ok {
		return apperror.Conflict("bill was modified concurrently")
	}
	return nil
}
