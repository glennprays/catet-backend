package service

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/glennprays/catet-backend/internal/apperror"
	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// Edit scopes accepted by UpdateAmount: change the nominal for this
// month and every month after it, or carve out a one-month exception.
const (
	ScopeFromMonth = "from_month"
	ScopeOnlyMonth = "only_month"
)

// BudgetService handles per-category budget rules with Go-computed
// spend. A budget is a chain of segments: each covers a run of months
// [starts_on, ends_before), the last one may be open-ended. Periods are
// YYYY-MM in Asia/Jakarta.
type BudgetService struct {
	repo    *repository.BudgetRepository
	cats    *repository.CategoryRepository
	summary *AnalyticsService
}

func NewBudgetService(repo *repository.BudgetRepository, cats *repository.CategoryRepository, summary *AnalyticsService) *BudgetService {
	return &BudgetService{repo: repo, cats: cats, summary: summary}
}

// normalize re-anchors the calendar dates Postgres handed back (UTC
// midnight) to Jakarta first-of-month, so every comparison in this file
// is between comparable anchors.
func normalize(rec *repository.BudgetRecord) {
	rec.StartsOn = JakartaMonthOf(rec.StartsOn)
	if rec.EndsBefore != nil {
		end := JakartaMonthOf(*rec.EndsBefore)
		rec.EndsBefore = &end
	}
}

func normalizeAll(recs []repository.BudgetRecord) []repository.BudgetRecord {
	for i := range recs {
		normalize(&recs[i])
	}
	return recs
}

// List returns BudgetRows (with subtree spend) for every segment
// covering the period, defaulting to the current Jakarta month.
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
	records, err := s.repo.ListCovering(ctx, start)
	if err != nil {
		return "", nil, apperror.Internal("failed to list budgets").Wrap(err)
	}
	normalizeAll(records)

	ended, err := s.repo.ListEndingAt(ctx, start)
	if err != nil {
		return "", nil, apperror.Internal("failed to list budgets").Wrap(err)
	}
	previous := make(map[uuid.UUID]int64, len(ended))
	for _, rec := range ended {
		previous[rec.CategoryID] = rec.AmountMinor
	}

	earliest, err := s.repo.EarliestStartByCategory(ctx)
	if err != nil {
		return "", nil, apperror.Internal("failed to list budgets").Wrap(err)
	}

	cats, err := s.cats.List(ctx, true)
	if err != nil {
		return "", nil, apperror.Internal("failed to load category").Wrap(err)
	}
	byID := make(map[uuid.UUID]entity.Category, len(cats))
	childrenOf := map[uuid.UUID][]uuid.UUID{}
	for _, cat := range cats {
		byID[cat.ID] = cat
		if cat.ParentID != nil {
			childrenOf[*cat.ParentID] = append(childrenOf[*cat.ParentID], cat.ID)
		}
	}

	spend, err := s.summary.CategorySpendMap(ctx, start, end)
	if err != nil {
		return "", nil, err
	}

	budgeted := make(map[uuid.UUID]struct{}, len(records))
	for _, rec := range records {
		budgeted[rec.CategoryID] = struct{}{}
	}

	rows := []entity.BudgetRow{}
	for _, rec := range records {
		cat, ok := byID[rec.CategoryID]
		if !ok {
			continue
		}
		row := entity.BudgetRow{
			ID:           rec.ID,
			CategoryID:   rec.CategoryID,
			CategoryName: cat.Name,
			Essential:    cat.Essential,
			AmountMinor:  rec.AmountMinor,
			SpentMinor:   subtreeSpend(rec.CategoryID, childrenOf, spend),
			FromPeriod:   JakartaMonthPeriod(rec.StartsOn),
			SincePeriod:  JakartaMonthPeriod(rec.StartsOn),
			Nested:       hasBudgetedAncestor(cat, byID, budgeted),
		}
		if rec.EndsBefore != nil {
			to := JakartaMonthPeriod(MonthBefore(*rec.EndsBefore))
			row.ToPeriod = &to
		}
		if since, ok := earliest[rec.CategoryID]; ok {
			row.SincePeriod = JakartaMonthPeriod(JakartaMonthOf(since))
		}
		if row.FromPeriod == p {
			if amount, ok := previous[rec.CategoryID]; ok {
				row.PreviousAmountMinor = &amount
			}
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return strings.ToLower(rows[i].CategoryName) < strings.ToLower(rows[j].CategoryName)
	})
	return p, rows, nil
}

// subtreeSpend sums the category's own spend plus every descendant's —
// archived children included, because their history still counts
// against the parent's limit.
func subtreeSpend(root uuid.UUID, childrenOf map[uuid.UUID][]uuid.UUID, spend map[uuid.UUID]int64) int64 {
	var total int64
	stack := []uuid.UUID{root}
	seen := map[uuid.UUID]struct{}{root: {}}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		total += spend[id]
		for _, child := range childrenOf[id] {
			if _, dup := seen[child]; dup {
				continue
			}
			seen[child] = struct{}{}
			stack = append(stack, child)
		}
	}
	return total
}

// hasBudgetedAncestor reports whether a category above this one already
// carries a budget in the same period; its spend is then counted twice
// and the row must stay out of the totals.
func hasBudgetedAncestor(cat entity.Category, byID map[uuid.UUID]entity.Category, budgeted map[uuid.UUID]struct{}) bool {
	parent := cat.ParentID
	for i := 0; i < 32 && parent != nil; i++ {
		if _, ok := budgeted[*parent]; ok {
			return true
		}
		next, ok := byID[*parent]
		if !ok {
			return false
		}
		parent = next.ParentID
	}
	return false
}

// rowFor rebuilds one row through the List pipeline so spend, nesting
// and the previous nominal are the same numbers a refresh would show.
func (s *BudgetService) rowFor(ctx context.Context, period string, id uuid.UUID) (*entity.BudgetRow, error) {
	_, rows, err := s.List(ctx, &period)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].ID == id {
			return &rows[i], nil
		}
	}
	return nil, apperror.Internal("budget disappeared after write")
}

// Create starts a budget rule in period. recurring rules run until the
// user stops them; a non-recurring one covers that single month. A rule
// created under an already-scheduled later segment is truncated at it.
func (s *BudgetService) Create(ctx context.Context, categoryID uuid.UUID, period string, amountMinor int64, recurring bool) (*entity.BudgetRow, error) {
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
	if cat.ArchivedAt != nil {
		return nil, apperror.Validation("budget validation failed").
			WithDetails(apperror.FieldError{Field: "category_id", Rule: "archived", Message: "category is archived"})
	}

	startsOn, err := JakartaMonthStart(period)
	if err != nil {
		return nil, err
	}
	var endsBefore *time.Time
	if recurring {
		chain, err := s.repo.ListByCategory(ctx, categoryID)
		if err != nil {
			return nil, apperror.Internal("failed to load budget").Wrap(err)
		}
		for _, rec := range normalizeAll(chain) {
			if rec.StartsOn.After(startsOn) {
				next := rec.StartsOn
				endsBefore = &next
				break
			}
		}
	} else {
		next := MonthAfter(startsOn)
		endsBefore = &next
	}

	created, err := s.repo.Insert(ctx, categoryID, startsOn, endsBefore, amountMinor)
	if err != nil {
		if isExclusionViolation(err) || isUniqueViolation(err) {
			return nil, apperror.Conflict("budget already covers this month for the category")
		}
		return nil, apperror.Internal("failed to create budget").Wrap(err)
	}
	return s.rowFor(ctx, period, created.ID)
}

// segmentCovering loads the segment and checks it really covers period;
// the dashboard always edits the row it is looking at.
func (s *BudgetService) segmentCovering(ctx context.Context, id uuid.UUID, period string) (*repository.BudgetRecord, time.Time, error) {
	month, err := JakartaMonthStart(period)
	if err != nil {
		return nil, time.Time{}, err
	}
	seg, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, time.Time{}, apperror.Internal("failed to load budget").Wrap(err)
	}
	if seg == nil {
		return nil, time.Time{}, apperror.NotFound("budget not found")
	}
	normalize(seg)
	if month.Before(seg.StartsOn) || (seg.EndsBefore != nil && !month.Before(*seg.EndsBefore)) {
		return nil, time.Time{}, apperror.Validation("budget validation failed").
			WithDetails(apperror.FieldError{Field: "period", Rule: "covered", Message: "budget does not cover this month"})
	}
	return seg, month, nil
}

// UpdateAmount changes the nominal from period on ("from_month") or for
// that single month ("only_month"), splitting the chain as needed.
func (s *BudgetService) UpdateAmount(ctx context.Context, id uuid.UUID, period, editScope string, amountMinor int64) (*entity.BudgetRow, error) {
	seg, month, err := s.segmentCovering(ctx, id, period)
	if err != nil {
		return nil, err
	}
	next := MonthAfter(month)

	resultID := seg.ID
	err = s.repo.WithTx(ctx, func(tx *repository.BudgetRepository) error {
		switch editScope {
		case ScopeOnlyMonth:
			if month.Equal(seg.StartsOn) && seg.EndsBefore != nil && seg.EndsBefore.Equal(next) {
				return tx.SetAmount(ctx, seg.ID, amountMinor)
			}
			if month.After(seg.StartsOn) {
				if err := tx.SetBounds(ctx, seg.ID, seg.StartsOn, &month); err != nil {
					return err
				}
			} else if _, err := tx.Delete(ctx, seg.ID); err != nil {
				return err
			}
			created, err := tx.Insert(ctx, seg.CategoryID, month, &next, amountMinor)
			if err != nil {
				return err
			}
			resultID = created.ID
			if seg.EndsBefore == nil || seg.EndsBefore.After(next) {
				if _, err := tx.Insert(ctx, seg.CategoryID, next, seg.EndsBefore, seg.AmountMinor); err != nil {
					return err
				}
			}
			return nil

		case ScopeFromMonth:
			chain, err := tx.ListByCategory(ctx, seg.CategoryID)
			if err != nil {
				return err
			}
			normalizeAll(chain)
			// The run keeps whatever end it already had: editing the
			// nominal is not a way to extend or cut a rule.
			var end *time.Time
			if len(chain) > 0 {
				end = chain[len(chain)-1].EndsBefore
			}
			if _, err := tx.DeleteStartingFrom(ctx, seg.CategoryID, month); err != nil {
				return err
			}
			if month.After(seg.StartsOn) {
				if err := tx.SetBounds(ctx, seg.ID, seg.StartsOn, &month); err != nil {
					return err
				}
			}
			created, err := tx.Insert(ctx, seg.CategoryID, month, end, amountMinor)
			if err != nil {
				return err
			}
			resultID = created.ID
			return nil

		default:
			return apperror.Validation("budget validation failed").
				WithDetails(apperror.FieldError{Field: "scope", Rule: "oneof", Message: "scope must be from_month or only_month"})
		}
	})
	if err != nil {
		var appErr *apperror.Error
		if errors.As(err, &appErr) {
			return nil, appErr
		}
		if isExclusionViolation(err) {
			return nil, apperror.Conflict("budget already covers this month for the category")
		}
		return nil, apperror.Internal("failed to update budget").Wrap(err)
	}
	return s.rowFor(ctx, period, resultID)
}

// Stop ends the rule at the start of period: that month and every month
// after it drop the budget, earlier months stay as history.
func (s *BudgetService) Stop(ctx context.Context, id uuid.UUID, period string) error {
	seg, month, err := s.segmentCovering(ctx, id, period)
	if err != nil {
		return err
	}
	err = s.repo.WithTx(ctx, func(tx *repository.BudgetRepository) error {
		if _, err := tx.DeleteStartingFrom(ctx, seg.CategoryID, month); err != nil {
			return err
		}
		if month.After(seg.StartsOn) {
			return tx.SetBounds(ctx, seg.ID, seg.StartsOn, &month)
		}
		return nil
	})
	if err != nil {
		return apperror.Internal("failed to stop budget").Wrap(err)
	}
	return nil
}

// DeleteAll removes the category's whole budget history, every segment.
func (s *BudgetService) DeleteAll(ctx context.Context, id uuid.UUID) error {
	seg, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return apperror.Internal("failed to load budget").Wrap(err)
	}
	if seg == nil {
		return apperror.NotFound("budget not found")
	}
	if _, err := s.repo.DeleteByCategory(ctx, seg.CategoryID); err != nil {
		return apperror.Internal("failed to delete budget").Wrap(err)
	}
	return nil
}

// isUniqueViolation reports Postgres SQLSTATE 23505.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// isExclusionViolation reports Postgres SQLSTATE 23P01 — two segments of
// one category claiming the same month.
func isExclusionViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23P01"
}
