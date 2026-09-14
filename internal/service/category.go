package service

import (
	"context"

	"github.com/glennprays/catet-backend/internal/apperror"
	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/google/uuid"
)

// CategoryService handles category CRUD with the archive guard
// (409 while a budget in the current period uses the category).
type CategoryService struct {
	repo *repository.CategoryRepository
}

func NewCategoryService(repo *repository.CategoryRepository) *CategoryService {
	return &CategoryService{repo: repo}
}

func (s *CategoryService) List(ctx context.Context, includeArchived bool) ([]entity.Category, error) {
	cats, err := s.repo.List(ctx, includeArchived)
	if err != nil {
		return nil, apperror.Internal("failed to list categories").Wrap(err)
	}
	return cats, nil
}

// CreateInput is the parsed POST /categories body.
type CreateCategoryInput struct {
	Name      string
	ParentID  *uuid.UUID
	Icon      *string
	Essential bool
}

func (s *CategoryService) Create(ctx context.Context, in CreateCategoryInput) (*entity.Category, error) {
	if in.ParentID != nil {
		parent, err := s.repo.Get(ctx, *in.ParentID)
		if err != nil {
			return nil, apperror.Internal("failed to load parent category").Wrap(err)
		}
		if parent == nil {
			return nil, apperror.Validation("category validation failed").
				WithDetails(apperror.FieldError{Field: "parent_id", Rule: "exists", Message: "parent category not found"})
		}
	}
	created, err := s.repo.Create(ctx, &entity.Category{
		ID:        uuid.New(),
		Name:      in.Name,
		ParentID:  in.ParentID,
		Icon:      in.Icon,
		Essential: in.Essential,
	})
	if err != nil {
		return nil, apperror.Internal("failed to create category").Wrap(err)
	}
	return created, nil
}

// UpdateInput is the parsed PATCH /categories/{id} body.
type UpdateCategoryInput struct {
	Name      *string
	ParentID  **uuid.UUID
	Icon      *string
	Essential *bool
}

func (s *CategoryService) Update(ctx context.Context, id uuid.UUID, in UpdateCategoryInput) (*entity.Category, error) {
	existing, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, apperror.Internal("failed to load category").Wrap(err)
	}
	if existing == nil {
		return nil, apperror.NotFound("category not found")
	}
	// Guard re-parent cycles: walk up the (new) parent chain.
	if in.ParentID != nil && *in.ParentID != nil {
		if err := s.guardCycle(ctx, id, **in.ParentID); err != nil {
			return nil, err
		}
	}
	updated, err := s.repo.Update(ctx, id, in.Name, in.ParentID, in.Icon, in.Essential)
	if err != nil {
		return nil, apperror.Internal("failed to update category").Wrap(err)
	}
	return updated, nil
}

func (s *CategoryService) guardCycle(ctx context.Context, id, newParent uuid.UUID) error {
	current := newParent
	for i := 0; i < 32 && current != uuid.Nil; i++ {
		if current == id {
			return apperror.Validation("category validation failed").
				WithDetails(apperror.FieldError{Field: "parent_id", Rule: "acyclic", Message: "cannot re-parent under a descendant"})
		}
		parent, err := s.repo.Get(ctx, current)
		if err != nil {
			return apperror.Internal("failed to walk category tree").Wrap(err)
		}
		if parent == nil || parent.ParentID == nil {
			return nil
		}
		current = *parent.ParentID
	}
	return nil
}

// Archive soft-deletes; blocked while a budget in the current Jakarta
// month references the category.
func (s *CategoryService) Archive(ctx context.Context, id uuid.UUID) error {
	existing, err := s.repo.Get(ctx, id)
	if err != nil {
		return apperror.Internal("failed to load category").Wrap(err)
	}
	if existing == nil {
		return apperror.NotFound("category not found")
	}
	period := JakartaMonthPeriod(JakartaNow())
	hasBudget, err := s.repo.HasBudgetInPeriod(ctx, id, period)
	if err != nil {
		return apperror.Internal("failed to check budgets").Wrap(err)
	}
	if hasBudget {
		return apperror.Conflict("category is used by a budget in the current period")
	}
	if _, err := s.repo.Archive(ctx, id); err != nil {
		return apperror.Internal("failed to archive category").Wrap(err)
	}
	return nil
}
