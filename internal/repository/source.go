package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SourceRepository persists sources and computes their balances from
// the legs ledger (balance is never stored — ADR-0001).
type SourceRepository struct {
	db *pgxpool.Pool
}

func NewSourceRepository(db *pgxpool.Pool) *SourceRepository {
	return &SourceRepository{db: db}
}

const sourceColumns = `id, name, type, is_debt, currency, archived_at, created_at`

func scanSource(row pgx.Row) (*entity.Source, error) {
	var s entity.Source
	err := row.Scan(&s.ID, &s.Name, &s.Type, &s.IsDebt, &s.Currency, &s.ArchivedAt, &s.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (r *SourceRepository) Create(ctx context.Context, s *entity.Source) (*entity.Source, error) {
	return scanSource(r.db.QueryRow(ctx,
		`INSERT INTO sources (id, name, type, is_debt, currency) VALUES ($1, $2, $3, $4, $5)
		 RETURNING `+sourceColumns,
		s.ID, s.Name, s.Type, s.IsDebt, s.Currency,
	))
}

func (r *SourceRepository) Get(ctx context.Context, id uuid.UUID) (*entity.Source, error) {
	return scanSource(r.db.QueryRow(ctx,
		`SELECT `+sourceColumns+` FROM sources WHERE id = $1`, id,
	))
}

func (r *SourceRepository) List(ctx context.Context, includeArchived bool) ([]entity.Source, error) {
	where := ""
	if !includeArchived {
		where = ` WHERE archived_at IS NULL`
	}
	rows, err := r.db.Query(ctx,
		`SELECT `+sourceColumns+` FROM sources`+where+` ORDER BY created_at ASC, name ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []entity.Source{}
	for rows.Next() {
		var s entity.Source
		if err := rows.Scan(&s.ID, &s.Name, &s.Type, &s.IsDebt, &s.Currency, &s.ArchivedAt, &s.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Update applies name/is_debt changes; nil = unchanged.
func (r *SourceRepository) Update(ctx context.Context, id uuid.UUID, name *string, isDebt *bool) (*entity.Source, error) {
	return scanSource(r.db.QueryRow(ctx,
		`UPDATE sources SET
			name = COALESCE($2, name),
			is_debt = COALESCE($3, is_debt)
		 WHERE id = $1
		 RETURNING `+sourceColumns,
		id, name, isDebt,
	))
}

// Archive soft-deletes a source.
func (r *SourceRepository) Archive(ctx context.Context, id uuid.UUID) (*entity.Source, error) {
	return scanSource(r.db.QueryRow(ctx,
		`UPDATE sources SET archived_at = now() WHERE id = $1 AND archived_at IS NULL
		 RETURNING `+sourceColumns,
		id,
	))
}

// HasActiveBills reports whether any active bill references the source.
func (r *SourceRepository) HasActiveBills(ctx context.Context, id uuid.UUID) (bool, error) {
	var exists bool
	err := r.db.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM bills WHERE linked_source_id = $1 AND active)`,
		id,
	).Scan(&exists)
	return exists, err
}

// Balances returns Σ non-voided legs per source. Sources without legs
// are absent (treat as zero).
func (r *SourceRepository) Balances(ctx context.Context) (map[uuid.UUID]int64, error) {
	rows, err := r.db.Query(ctx,
		`SELECT l.source_id, COALESCE(SUM(l.signed_amount), 0)
		 FROM legs l
		 JOIN transactions t ON t.id = l.transaction_id
		 WHERE l.source_id IS NOT NULL AND t.voided_at IS NULL
		 GROUP BY l.source_id`,
	)
	if err != nil {
		return nil, fmt.Errorf("query balances: %w", err)
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
