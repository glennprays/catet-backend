package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/scope"
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

// Create parks a new tool call. The bot writes here whenever its rules
// refuse to guess; the dashboard resolves it later.
func (r *PendingActionRepository) Create(ctx context.Context, kind string, params json.RawMessage, summary string) (*entity.PendingAction, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, err
	}
	if len(params) == 0 {
		params = json.RawMessage(`{}`)
	}
	var p entity.PendingAction
	err = r.db.QueryRow(ctx,
		`INSERT INTO pending_actions (user_id, kind, params, summary)
		 VALUES ($1,$2,$3,$4)
		 RETURNING id, kind, params, summary, action, created_at, resolved_at`,
		uid, kind, params, summary,
	).Scan(&p.ID, &p.Kind, &p.Params, &p.Summary, &p.Action, &p.CreatedAt, &p.ResolvedAt)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// FindOrCreateByExternalID atomically reuses every prior action for a
// capture line, including resolved and discarded rows. The transaction
// advisory lock closes the stale-worker race without adding a schema key;
// the external_id is already persisted in the action JSON.
func (r *PendingActionRepository) FindOrCreateByExternalID(
	ctx context.Context, kind, externalID string, params json.RawMessage, summary string,
) (*entity.PendingAction, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, err
	}
	if externalID == "" {
		return r.Create(ctx, kind, params, summary)
	}
	if len(params) == 0 {
		params = json.RawMessage(`{}`)
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	lockKey := fmt.Sprintf("%s:%s:%s", uid, kind, externalID)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, lockKey); err != nil {
		return nil, err
	}
	var p entity.PendingAction
	err = tx.QueryRow(ctx,
		`SELECT id, kind, params, summary, action, created_at, resolved_at
		 FROM pending_actions
		 WHERE user_id = $1 AND kind = $2 AND params->>'external_id' = $3
		 ORDER BY created_at ASC, id ASC
		 LIMIT 1`,
		uid, kind, externalID,
	).Scan(&p.ID, &p.Kind, &p.Params, &p.Summary, &p.Action, &p.CreatedAt, &p.ResolvedAt)
	if err == nil {
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return &p, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	err = tx.QueryRow(ctx,
		`INSERT INTO pending_actions (user_id, kind, params, summary)
		 VALUES ($1,$2,$3,$4)
		 RETURNING id, kind, params, summary, action, created_at, resolved_at`,
		uid, kind, params, summary,
	).Scan(&p.ID, &p.Kind, &p.Params, &p.Summary, &p.Action, &p.CreatedAt, &p.ResolvedAt)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &p, nil
}

// ListOpen returns unresolved items, newest first.
func (r *PendingActionRepository) ListOpen(ctx context.Context) ([]entity.PendingAction, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Query(ctx,
		`SELECT id, kind, params, summary, action, created_at, resolved_at
		 FROM pending_actions WHERE user_id = $1 AND resolved_at IS NULL
		 ORDER BY created_at DESC`,
		uid,
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
	uid, err := scope.UserID(ctx)
	if err != nil {
		return 0, err
	}
	var n int64
	err = r.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM pending_actions WHERE user_id = $1 AND resolved_at IS NULL`,
		uid,
	).Scan(&n)
	return n, err
}

// GetOpen loads one unresolved item; nil when missing/already resolved.
func (r *PendingActionRepository) GetOpen(ctx context.Context, id uuid.UUID) (*entity.PendingAction, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, err
	}
	var p entity.PendingAction
	err = r.db.QueryRow(ctx,
		`SELECT id, kind, params, summary, action, created_at, resolved_at
		 FROM pending_actions WHERE id = $1 AND user_id = $2 AND resolved_at IS NULL`,
		id, uid,
	).Scan(&p.ID, &p.Kind, &p.Params, &p.Summary, &p.Action, &p.CreatedAt, &p.ResolvedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// UpdateParams rewrites an open draft in place. The chat edits a parked
// receipt — a Source, a category, a split payment — before confirming
// it, and the dashboard must see the same draft the user is talking
// about. A row that is no longer open is never rewritten.
func (r *PendingActionRepository) UpdateParams(ctx context.Context, id uuid.UUID, params json.RawMessage, summary string) error {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return err
	}
	tag, err := r.db.Exec(ctx,
		`UPDATE pending_actions SET params = $2, summary = $3
		 WHERE id = $1 AND user_id = $4 AND resolved_at IS NULL`,
		id, params, summary, uid,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// Resolve marks the item resolved with the chosen action
// (confirm = execute, discard = drop). Returns false when the item
// was not open.
func (r *PendingActionRepository) Resolve(ctx context.Context, id uuid.UUID, action string) (bool, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return false, err
	}
	tag, err := r.db.Exec(ctx,
		`UPDATE pending_actions SET action = $2, resolved_at = $3
		 WHERE id = $1 AND user_id = $4 AND resolved_at IS NULL`,
		id, action, time.Now().UTC(), uid,
	)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
