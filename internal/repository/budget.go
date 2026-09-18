package repository

import (
	"context"
	"errors"
	"time"

	"github.com/glennprays/catet-backend/internal/scope"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// budgetQuerier is the slice of pgx both *pgxpool.Pool and pgx.Tx
// implement, so WithTx can hand the same methods a transaction-bound
// copy of the repository.
type budgetQuerier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// BudgetRepository persists per-category budget segments. A segment
// covers the half-open month range [starts_on, ends_before); a NULL
// ends_before runs until the user stops it. Spend is computed in the
// analytics queries and joined in the service.
type BudgetRepository struct {
	db *pgxpool.Pool
	q  budgetQuerier
}

func NewBudgetRepository(db *pgxpool.Pool) *BudgetRepository {
	return &BudgetRepository{db: db, q: db}
}

// BudgetRecord is one stored segment before names/spend are joined.
// StartsOn and EndsBefore are first-of-month calendar dates as Postgres
// returned them (UTC midnight); the service re-anchors them to Jakarta.
type BudgetRecord struct {
	ID          uuid.UUID
	CategoryID  uuid.UUID
	AmountMinor int64
	StartsOn    time.Time
	EndsBefore  *time.Time
}

const budgetCols = `id, category_id, amount_minor, starts_on, ends_before`

// WithTx runs fn against a transaction-bound copy of the repository and
// commits when it returns nil. Every multi-statement reshaping of a
// segment chain goes through here: a half-applied split would leave the
// category with a gap or an overlap.
func (r *BudgetRepository) WithTx(ctx context.Context, fn func(*BudgetRepository) error) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(&BudgetRepository{db: r.db, q: tx}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// dateArg renders a calendar date for a `date` parameter without the
// timezone drift a time.Time round-trip would add.
func dateArg(t time.Time) string { return t.Format("2006-01-02") }

func (r *BudgetRepository) scanRecords(rows pgx.Rows) ([]BudgetRecord, error) {
	defer rows.Close()

	out := []BudgetRecord{}
	for rows.Next() {
		var rec BudgetRecord
		if err := rows.Scan(&rec.ID, &rec.CategoryID, &rec.AmountMinor, &rec.StartsOn, &rec.EndsBefore); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// ListCovering returns every segment whose range contains month.
func (r *BudgetRepository) ListCovering(ctx context.Context, month time.Time) ([]BudgetRecord, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := r.q.Query(ctx,
		`SELECT `+budgetCols+` FROM budgets
		 WHERE user_id = $1 AND starts_on <= $2 AND (ends_before IS NULL OR ends_before > $2)
		 ORDER BY starts_on ASC, id ASC`,
		uid, dateArg(month),
	)
	if err != nil {
		return nil, err
	}
	return r.scanRecords(rows)
}

// ListEndingAt returns the segments that stopped exactly when month
// begins — the previous nominal a changed budget is compared against.
func (r *BudgetRepository) ListEndingAt(ctx context.Context, month time.Time) ([]BudgetRecord, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := r.q.Query(ctx,
		`SELECT `+budgetCols+` FROM budgets
		 WHERE user_id = $1 AND ends_before = $2
		 ORDER BY starts_on ASC, id ASC`,
		uid, dateArg(month),
	)
	if err != nil {
		return nil, err
	}
	return r.scanRecords(rows)
}

// ListByCategory returns the whole segment chain of one category,
// oldest first — the shape the edit and stop operations reshape.
func (r *BudgetRepository) ListByCategory(ctx context.Context, categoryID uuid.UUID) ([]BudgetRecord, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := r.q.Query(ctx,
		`SELECT `+budgetCols+` FROM budgets
		 WHERE user_id = $1 AND category_id = $2
		 ORDER BY starts_on ASC, id ASC`,
		uid, categoryID,
	)
	if err != nil {
		return nil, err
	}
	return r.scanRecords(rows)
}

// EarliestStartByCategory maps category_id → the first month the
// category was ever budgeted (the "since" a recurring row reports).
func (r *BudgetRepository) EarliestStartByCategory(ctx context.Context) (map[uuid.UUID]time.Time, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := r.q.Query(ctx,
		`SELECT category_id, MIN(starts_on) FROM budgets
		 WHERE user_id = $1 GROUP BY category_id`,
		uid,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[uuid.UUID]time.Time{}
	for rows.Next() {
		var id uuid.UUID
		var start time.Time
		if err := rows.Scan(&id, &start); err != nil {
			return nil, err
		}
		out[id] = start
	}
	return out, rows.Err()
}

// GetByID loads one segment regardless of period; nil when missing —
// another user's budget is missing as far as this caller is concerned.
func (r *BudgetRepository) GetByID(ctx context.Context, id uuid.UUID) (*BudgetRecord, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, err
	}
	var rec BudgetRecord
	err = r.q.QueryRow(ctx,
		`SELECT `+budgetCols+` FROM budgets WHERE id = $1 AND user_id = $2`,
		id, uid,
	).Scan(&rec.ID, &rec.CategoryID, &rec.AmountMinor, &rec.StartsOn, &rec.EndsBefore)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

// Insert adds one segment. endsBefore nil means "runs until stopped".
func (r *BudgetRepository) Insert(ctx context.Context, categoryID uuid.UUID, startsOn time.Time, endsBefore *time.Time, amountMinor int64) (*BudgetRecord, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, err
	}
	var end *string
	if endsBefore != nil {
		s := dateArg(*endsBefore)
		end = &s
	}
	var rec BudgetRecord
	err = r.q.QueryRow(ctx,
		`INSERT INTO budgets (user_id, category_id, amount_minor, starts_on, ends_before, auto_suggested)
		 VALUES ($1, $2, $3, $4, $5, false)
		 RETURNING `+budgetCols,
		uid, categoryID, amountMinor, dateArg(startsOn), end,
	).Scan(&rec.ID, &rec.CategoryID, &rec.AmountMinor, &rec.StartsOn, &rec.EndsBefore)
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

// SetBounds re-dates one segment; pgx.ErrNoRows when it is not the
// caller's.
func (r *BudgetRepository) SetBounds(ctx context.Context, id uuid.UUID, startsOn time.Time, endsBefore *time.Time) error {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return err
	}
	var end *string
	if endsBefore != nil {
		s := dateArg(*endsBefore)
		end = &s
	}
	tag, err := r.q.Exec(ctx,
		`UPDATE budgets SET starts_on = $2, ends_before = $3 WHERE id = $1 AND user_id = $4`,
		id, dateArg(startsOn), end, uid,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

func (r *BudgetRepository) SetAmount(ctx context.Context, id uuid.UUID, amountMinor int64) error {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return err
	}
	tag, err := r.q.Exec(ctx,
		`UPDATE budgets SET amount_minor = $2 WHERE id = $1 AND user_id = $3`,
		id, amountMinor, uid,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

func (r *BudgetRepository) Delete(ctx context.Context, id uuid.UUID) (bool, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return false, err
	}
	tag, err := r.q.Exec(ctx, `DELETE FROM budgets WHERE id = $1 AND user_id = $2`, id, uid)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// DeleteByCategory drops the category's whole budget history.
func (r *BudgetRepository) DeleteByCategory(ctx context.Context, categoryID uuid.UUID) (int64, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return 0, err
	}
	tag, err := r.q.Exec(ctx,
		`DELETE FROM budgets WHERE category_id = $1 AND user_id = $2`, categoryID, uid,
	)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// DeleteStartingFrom drops every segment of the category that begins on
// or after month; the one straddling month is re-dated by the caller.
func (r *BudgetRepository) DeleteStartingFrom(ctx context.Context, categoryID uuid.UUID, month time.Time) (int64, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return 0, err
	}
	tag, err := r.q.Exec(ctx,
		`DELETE FROM budgets WHERE user_id = $1 AND category_id = $2 AND starts_on >= $3`,
		uid, categoryID, dateArg(month),
	)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
