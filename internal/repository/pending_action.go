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

// PendingActionRepository backs the Hold Queue: Tier 1-3 tool calls
// parked until named confirmation (ADR-0003).
type PendingActionRepository struct {
	db *pgxpool.Pool
}

func NewPendingActionRepository(db *pgxpool.Pool) *PendingActionRepository {
	return &PendingActionRepository{db: db}
}

// ListOpen returns unresolved items, newest first.
func (r *PendingActionRepository) ListOpen(ctx context.Context) ([]entity.PendingAction, error) {
	rows, err := r.db.Query(ctx,
		`SELECT id, kind, params, summary, action, created_at, resolved_at
		 FROM pending_actions WHERE resolved_at IS NULL
		 ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []entity.PendingAction{}
	for rows.Next() {
		var p entity.PendingAction
		if err := rows.Scan(&p.ID, &p.Kind, &p.Params, &p.Summary, &p.Action, &p.CreatedAt, &p.ResolvedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CountOpen returns the badge count.
func (r *PendingActionRepository) CountOpen(ctx context.Context) (int64, error) {
	var n int64
	err := r.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM pending_actions WHERE resolved_at IS NULL`,
	).Scan(&n)
	return n, err
}

// GetOpen loads one unresolved item; nil when missing/already resolved.
func (r *PendingActionRepository) GetOpen(ctx context.Context, id uuid.UUID) (*entity.PendingAction, error) {
	var p entity.PendingAction
	err := r.db.QueryRow(ctx,
		`SELECT id, kind, params, summary, action, created_at, resolved_at
		 FROM pending_actions WHERE id = $1 AND resolved_at IS NULL`,
		id,
	).Scan(&p.ID, &p.Kind, &p.Params, &p.Summary, &p.Action, &p.CreatedAt, &p.ResolvedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// Resolve marks the item resolved with the chosen action
// (confirm = execute, discard = drop). Returns false when the item
// was not open.
func (r *PendingActionRepository) Resolve(ctx context.Context, id uuid.UUID, action string) (bool, error) {
	tag, err := r.db.Exec(ctx,
		`UPDATE pending_actions SET action = $2, resolved_at = $3
		 WHERE id = $1 AND resolved_at IS NULL`,
		id, action, time.Now().UTC(),
	)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
