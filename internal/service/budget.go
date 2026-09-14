package service

import (
	"context"
	"errors"

	"github.com/glennprays/catet-backend/internal/apperror"
	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// BudgetService handles per-category monthly budgets with Go-computed
// spend. Periods are YYYY-MM in Asia/Jakarta.
type BudgetService struct {
	repo    *repository.BudgetRepository
	cats    *repository.CategoryRepository
	summary *AnalyticsService
}

func NewBudgetService(repo *repository.BudgetRepository, cats *repository.CategoryRepository, summary *AnalyticsService) *BudgetService {
	return &BudgetService{repo: repo, cats: cats, summary: summary}
}

// List returns BudgetRows (with spent) for the period, defaulting to
// the current Jakarta month.
func (s *BudgetService) List(ctx context.Context, period *string) (string, []entity.BudgetRow, error) {
	p := JakartaMonthPeriod(JakartaNow())
	if period != nil && *period != "" {
		if _, _, err := JakartaMonthBounds(*period); err != nil {
			return "", nil, err
		}
		p = *period
	}

	start, end, err := JakartaMonthBounds(p)
	if err != nil {
		return "", nil, err
	}
	records, err := s.repo.List(ctx, p)
	if err != nil {
		return "", nil, apperror.Internal("failed to list budgets").Wrap(err)
	}
	spend, err := s.summary.CategorySpendMap(ctx, start, end)
	if err != nil {
		return "", nil, err
	}

	rows := []entity.BudgetRow{}
	for _, rec := range records {
		cat, err := s.cats.Get(ctx, rec.CategoryID)
		if err != nil {
			return "", nil, apperror.Internal("failed to load category").Wrap(err)
		}
		if cat == nil {
			continue
		}
		rows = append(rows, entity.BudgetRow{
			ID:           rec.ID,
			CategoryID:   rec.CategoryID,
			CategoryName: cat.Name,
			Essential:    cat.Essential,
			AmountMinor:  rec.AmountMinor,
			SpentMinor:   spend[rec.CategoryID],
		})
	}
	return p, rows, nil
}

// Create adds a budget row; duplicate (category, period) → 409.
func (s *BudgetService) Create(ctx context.Context, categoryID uuid.UUID, period string, amountMinor int64) (*entity.BudgetRow, error) {
	if _, _, err := JakartaMonthBounds(period); err != nil {
		return nil, err
	}
	cat, err := s.cats.Get(ctx, categoryID)
	if err != nil {
		return nil, apperror.Internal("failed to load category").Wrap(err)
	}
	if cat == nil {
		return nil, apperror.Validation("budget validation failed").
			WithDetails(apperror.FieldError{Field: "category_id", Rule: "exists", Message: "category not found"})
	}

	row := &entity.BudgetRow{CategoryID: categoryID, AmountMinor: amountMinor}
	created, err := s.repo.Create(ctx, row, period, false)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, apperror.Conflict("budget already exists for this category and period")
		}
		return nil, apperror.Internal("failed to create budget").Wrap(err)
	}
	created.CategoryName = cat.Name
	created.Essential = cat.Essential
	return created, nil
}

// UpdateAmount PATCHes the limit on one budget row.
func (s *BudgetService) UpdateAmount(ctx context.Context, id uuid.UUID, amountMinor int64) (*entity.BudgetRow, error) {
	rec, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, apperror.Internal("failed to load budget").Wrap(err)
	}
	if rec == nil {
		return nil, apperror.NotFound("budget not found")
	}
	if err := s.repo.UpdateAmount(ctx, id, amountMinor); err != nil {
		return nil, apperror.Internal("failed to update budget").Wrap(err)
	}
	return s.rowFor(ctx, rec.CategoryID, id, amountMinor)
}

func (s *BudgetService) rowFor(ctx context.Context, categoryID, id uuid.UUID, amount int64) (*entity.BudgetRow, error) {
	cat, err := s.cats.Get(ctx, categoryID)
	if err != nil {
		return nil, apperror.Internal("failed to load category").Wrap(err)
	}
	row := &entity.BudgetRow{ID: id, CategoryID: categoryID, AmountMinor: amount}
	if cat != nil {
		row.CategoryName = cat.Name
		row.Essential = cat.Essential
	}
	return row, nil
}

// Delete removes a budget row.
func (s *BudgetService) Delete(ctx context.Context, id uuid.UUID) error {
	ok, err := s.repo.Delete(ctx, id)
	if err != nil {
		return apperror.Internal("failed to delete budget").Wrap(err)
	}
	if !ok {
		return apperror.NotFound("budget not found")
	}
	return nil
}

// isUniqueViolation reports Postgres SQLSTATE 23505.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
