package repository

import (
	"context"
	"errors"
	"time"

	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// BudgetRepository persists per-category monthly budgets. Spend is
// computed in the analytics queries and joined in the service.
type BudgetRepository struct {
	db *pgxpool.Pool
}

func NewBudgetRepository(db *pgxpool.Pool) *BudgetRepository {
	return &BudgetRepository{db: db}
}

// BudgetRecord is the raw stored row before names/spend are joined.
type BudgetRecord struct {
	ID          uuid.UUID
	CategoryID  uuid.UUID
	AmountMinor int64
}

func (r *BudgetRepository) Create(ctx context.Context, b *entity.BudgetRow, period string, autoSuggested bool) (*entity.BudgetRow, error) {
	err := r.db.QueryRow(ctx,
		`INSERT INTO budgets (category_id, period, amount_minor, auto_suggested)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id`,
		b.CategoryID, period, b.AmountMinor, autoSuggested,
	).Scan(&b.ID)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func (r *BudgetRepository) List(ctx context.Context, period string) ([]BudgetRecord, error) {
	rows, err := r.db.Query(ctx,
		`SELECT id, category_id, amount_minor FROM budgets WHERE period = $1 ORDER BY id ASC`,
		period,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []BudgetRecord{}
	for rows.Next() {
		var rec BudgetRecord
		if err := rows.Scan(&rec.ID, &rec.CategoryID, &rec.AmountMinor); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// GetByID loads one budget regardless of period; nil when missing.
func (r *BudgetRepository) GetByID(ctx context.Context, id uuid.UUID) (*BudgetRecord, error) {
	var rec BudgetRecord
	err := r.db.QueryRow(ctx,
		`SELECT id, category_id, amount_minor FROM budgets WHERE id = $1`,
		id,
	).Scan(&rec.ID, &rec.CategoryID, &rec.AmountMinor)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

func (r *BudgetRepository) UpdateAmount(ctx context.Context, id uuid.UUID, amountMinor int64) error {
	tag, err := r.db.Exec(ctx,
		`UPDATE budgets SET amount_minor = $2 WHERE id = $1`,
		id, amountMinor,
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
	tag, err := r.db.Exec(ctx, `DELETE FROM budgets WHERE id = $1`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// SpendByCategory maps category_id → Σ expense category legs (the
// positive side) in [from, to), voided excluded. Reversals carry the
// mirrored negative leg and cancel naturally.
func (r *BudgetRepository) SpendByCategory(ctx context.Context, from, to time.Time) (map[uuid.UUID]int64, error) {
	rows, err := r.db.Query(ctx,
		`SELECT l.category_id, COALESCE(SUM(l.signed_amount), 0)
		 FROM legs l
		 JOIN transactions t ON t.id = l.transaction_id
		 WHERE t.type = 'expense' AND t.voided_at IS NULL
		   AND l.category_id IS NOT NULL AND l.signed_amount > 0
		   AND t.occurred_at >= $1 AND t.occurred_at < $2
		 GROUP BY l.category_id`,
		from, to,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[uuid.UUID]int64{}
	for rows.Next() {
		var id uuid.UUID
		var amount int64
		if err := rows.Scan(&id, &amount); err != nil {
			return nil, err
		}
		out[id] = amount
	}
	return out, rows.Err()
}
