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

// BillRepository persists recurring obligations.
type BillRepository struct {
	db *pgxpool.Pool
}

func NewBillRepository(db *pgxpool.Pool) *BillRepository {
	return &BillRepository{db: db}
}

const billColumns = `
	b.id, b.name, b.amount_min_minor, b.amount_max_minor, b.cadence,
	b.next_due_date, b.linked_source_id, s.name, b.category_id, c.name,
	b.active, b.created_at`

const billFrom = `
	FROM bills b
	LEFT JOIN sources s ON s.id = b.linked_source_id
	LEFT JOIN categories c ON c.id = b.category_id`

func scanBill(row pgx.Row) (*entity.Bill, error) {
	var b entity.Bill
	err := row.Scan(
		&b.ID, &b.Name, &b.AmountMinMinor, &b.AmountMaxMinor, &b.Cadence,
		&b.NextDueDate, &b.LinkedSourceID, &b.LinkedSourceName, &b.CategoryID, &b.CategoryName,
		&b.Active, &b.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func (r *BillRepository) Create(ctx context.Context, b *entity.Bill) (*entity.Bill, error) {
	// INSERT ... RETURNING cannot join, so insert then re-read via Get
	// to pick up the joined source/category names.
	var id uuid.UUID
	err := r.db.QueryRow(ctx,
		`INSERT INTO bills (id, name, amount_min_minor, amount_max_minor, cadence, next_due_date, linked_source_id, category_id)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		 RETURNING id`,
		b.ID, b.Name, b.AmountMinMinor, b.AmountMaxMinor, b.Cadence, b.NextDueDate, b.LinkedSourceID, b.CategoryID,
	).Scan(&id)
	if err != nil {
		return nil, err
	}
	return r.Get(ctx, id)
}

func (r *BillRepository) Get(ctx context.Context, id uuid.UUID) (*entity.Bill, error) {
	return scanBill(r.db.QueryRow(ctx,
		`SELECT `+billColumns+billFrom+` WHERE b.id = $1`, id,
	))
}

func (r *BillRepository) List(ctx context.Context) ([]entity.Bill, error) {
	rows, err := r.db.Query(ctx,
		`SELECT `+billColumns+billFrom+` ORDER BY b.active DESC, b.next_due_date ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []entity.Bill{}
	for rows.Next() {
		var b entity.Bill
		if err := rows.Scan(
			&b.ID, &b.Name, &b.AmountMinMinor, &b.AmountMaxMinor, &b.Cadence,
			&b.NextDueDate, &b.LinkedSourceID, &b.LinkedSourceName, &b.CategoryID, &b.CategoryName,
			&b.Active, &b.CreatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// Update applies any subset; nil = unchanged. Scanning goes through
// Get so the joined names stay fresh.
func (r *BillRepository) Update(ctx context.Context, id uuid.UUID, fields map[string]any) (*entity.Bill, error) {
	sets := make([]string, 0, len(fields))
	args := []any{id}
	i := 1
	for col, val := range fields {
		i++
		args = append(args, val)
		sets = append(sets, col+" = $"+itoa(i))
	}
	if len(sets) == 0 {
		return r.Get(ctx, id)
	}
	q := `UPDATE bills SET ` + joinComma(sets) + ` WHERE id = $1`
	tag, err := r.db.Exec(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, nil
	}
	return r.Get(ctx, id)
}

// SetActive deactivates a bill. Returns false when missing.
func (r *BillRepository) SetActive(ctx context.Context, id uuid.UUID, active bool) (bool, error) {
	tag, err := r.db.Exec(ctx, `UPDATE bills SET active = $2 WHERE id = $1`, id, active)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ReservedSum sums amount_max_minor of active bills whose
// next_due_date falls in [from, to) — the conservative reservation
// (band max) for Safe-to-Spend.
func (r *BillRepository) ReservedSum(ctx context.Context, from, to time.Time) (int64, error) {
	var sum int64
	err := r.db.QueryRow(ctx,
		`SELECT COALESCE(SUM(amount_max_minor), 0) FROM bills
		 WHERE active AND next_due_date >= $1 AND next_due_date < $2`,
		from, to,
	).Scan(&sum)
	return sum, err
}
