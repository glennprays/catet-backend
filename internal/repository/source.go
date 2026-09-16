package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/scope"
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
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, err
	}
	return scanSource(r.db.QueryRow(ctx,
		`INSERT INTO sources (id, name, type, is_debt, currency, user_id) VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING `+sourceColumns,
		s.ID, s.Name, s.Type, s.IsDebt, s.Currency, uid,
	))
}

func (r *SourceRepository) Get(ctx context.Context, id uuid.UUID) (*entity.Source, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, err
	}
	return scanSource(r.db.QueryRow(ctx,
		`SELECT `+sourceColumns+` FROM sources WHERE id = $1 AND user_id = $2`, id, uid,
	))
}

func (r *SourceRepository) List(ctx context.Context, includeArchived bool) ([]entity.Source, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, err
	}
	where := ` WHERE user_id = $1`
	if !includeArchived {
		where += ` AND archived_at IS NULL`
	}
	rows, err := r.db.Query(ctx,
		`SELECT `+sourceColumns+` FROM sources`+where+` ORDER BY created_at ASC, name ASC`,
		uid,
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

// Update applies name/type/is_debt changes; nil = unchanged.
func (r *SourceRepository) Update(ctx context.Context, id uuid.UUID, name *string, typ *string, isDebt *bool) (*entity.Source, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, err
	}
	return scanSource(r.db.QueryRow(ctx,
		`UPDATE sources SET
			name = COALESCE($2, name),
			type = COALESCE($3, type),
			is_debt = COALESCE($4, is_debt)
		 WHERE id = $1 AND user_id = $5
		 RETURNING `+sourceColumns,
		id, name, typ, isDebt, uid,
	))
}

// Unarchive lifts the soft delete. Returns nil when the source does
// not exist or is not archived.
func (r *SourceRepository) Unarchive(ctx context.Context, id uuid.UUID) (*entity.Source, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, err
	}
	return scanSource(r.db.QueryRow(ctx,
		`UPDATE sources SET archived_at = NULL WHERE id = $1 AND user_id = $2 AND archived_at IS NOT NULL
		 RETURNING `+sourceColumns,
		id, uid,
	))
}

// DeletePermanently drops the row for good. Bills linking the source
// keep existing — the link is a hint, so it is cleared first. Callers
// must have checked that no leg references the source.
func (r *SourceRepository) DeletePermanently(ctx context.Context, id uuid.UUID) (bool, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return false, err
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if _, err := tx.Exec(ctx,
		`UPDATE bills SET linked_source_id = NULL WHERE linked_source_id = $1 AND user_id = $2`,
		id, uid,
	); err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, `DELETE FROM sources WHERE id = $1 AND user_id = $2`, id, uid)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ClearAsPhotoDefault drops the source from the bot's photo default so
// an archived source is never proposed again (internal/bot/source.go).
func (r *SourceRepository) ClearAsPhotoDefault(ctx context.Context, id uuid.UUID) error {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return err
	}
	_, err = r.db.Exec(ctx,
		`UPDATE user_settings SET photo_default_source_id = NULL WHERE user_id = $2 AND photo_default_source_id = $1`,
		id, uid,
	)
	return err
}

// Archive soft-deletes a source.
func (r *SourceRepository) Archive(ctx context.Context, id uuid.UUID) (*entity.Source, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, err
	}
	return scanSource(r.db.QueryRow(ctx,
		`UPDATE sources SET archived_at = now() WHERE id = $1 AND user_id = $2 AND archived_at IS NULL
		 RETURNING `+sourceColumns,
		id, uid,
	))
}

// HasActiveBills reports whether any active bill references the source.
func (r *SourceRepository) HasActiveBills(ctx context.Context, id uuid.UUID) (bool, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return false, err
	}
	var exists bool
	err = r.db.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM bills WHERE linked_source_id = $1 AND user_id = $2 AND active)`,
		id, uid,
	).Scan(&exists)
	return exists, err
}

// Balances returns Σ non-voided legs per source. Sources without legs
// are absent (treat as zero).
func (r *SourceRepository) Balances(ctx context.Context) (map[uuid.UUID]int64, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Query(ctx,
		`SELECT l.source_id, COALESCE(SUM(l.signed_amount), 0)
		 FROM legs l
		 JOIN transactions t ON t.id = l.transaction_id
		 WHERE l.source_id IS NOT NULL AND t.voided_at IS NULL AND t.user_id = $1
		 GROUP BY l.source_id`,
		uid,
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

// WithHistory reports which sources are referenced by at least one
// leg. Voided transactions count — history is history, and a voided
// row still points at the source.
func (r *SourceRepository) WithHistory(ctx context.Context) (map[uuid.UUID]bool, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Query(ctx,
		`SELECT DISTINCT l.source_id
		 FROM legs l
		 JOIN transactions t ON t.id = l.transaction_id
		 WHERE l.source_id IS NOT NULL AND t.user_id = $1`,
		uid,
	)
	if err != nil {
		return nil, fmt.Errorf("query source history: %w", err)
	}
	defer rows.Close()

	out := map[uuid.UUID]bool{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}
