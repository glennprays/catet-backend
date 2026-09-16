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

// InviteRepository persists phone-bound registration invites. Like
// users, invites predate any tenant, so nothing here reads
// internal/scope.
type InviteRepository struct {
	db *pgxpool.Pool
}

func NewInviteRepository(db *pgxpool.Pool) *InviteRepository {
	return &InviteRepository{db: db}
}

const inviteColumns = `id, phone, created_by, expires_at, used_at, created_at`

func scanInvite(row pgx.Row) (*entity.Invite, error) {
	var i entity.Invite
	err := row.Scan(&i.ID, &i.Phone, &i.CreatedBy, &i.ExpiresAt, &i.UsedAt, &i.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &i, nil
}

// Create issues an invite, replacing whatever open one the number had:
// the stored hash cannot be turned back into a link, so re-inviting has
// to mint a new token.
func (r *InviteRepository) Create(
	ctx context.Context, phone, tokenHash string, createdBy uuid.UUID, expiresAt time.Time,
) (*entity.Invite, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`DELETE FROM invites WHERE phone = $1 AND used_at IS NULL`, phone,
	); err != nil {
		return nil, err
	}
	inv, err := scanInvite(tx.QueryRow(ctx,
		`INSERT INTO invites (phone, token_hash, created_by, expires_at)
		 VALUES ($1,$2,$3,$4)
		 RETURNING `+inviteColumns,
		phone, tokenHash, createdBy, expiresAt,
	))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return inv, nil
}

// GetOpenByTokenHash resolves a register link; nil when the token is
// unknown, already used or expired.
func (r *InviteRepository) GetOpenByTokenHash(ctx context.Context, hash string) (*entity.Invite, error) {
	return scanInvite(r.db.QueryRow(ctx,
		`SELECT `+inviteColumns+` FROM invites
		 WHERE token_hash = $1 AND used_at IS NULL AND expires_at > now()`,
		hash,
	))
}

// OpenByPhoneDigits is the bot's lookup: an unknown sender who was
// invited gets the link resent instead of silence.
func (r *InviteRepository) OpenByPhoneDigits(ctx context.Context, digits string) (*entity.Invite, error) {
	if digits == "" {
		return nil, nil
	}
	return scanInvite(r.db.QueryRow(ctx,
		`SELECT `+inviteColumns+` FROM invites
		 WHERE phone_digits = $1 AND used_at IS NULL AND expires_at > now()`,
		digits,
	))
}

// ListOpen lists invites still waiting to be accepted, newest first.
func (r *InviteRepository) ListOpen(ctx context.Context) ([]entity.Invite, error) {
	rows, err := r.db.Query(ctx,
		`SELECT `+inviteColumns+` FROM invites
		 WHERE used_at IS NULL AND expires_at > now()
		 ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []entity.Invite{}
	for rows.Next() {
		var i entity.Invite
		if err := rows.Scan(&i.ID, &i.Phone, &i.CreatedBy, &i.ExpiresAt, &i.UsedAt, &i.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// MarkUsed closes an invite the moment its account exists.
func (r *InviteRepository) MarkUsed(ctx context.Context, id uuid.UUID) error {
	tag, err := r.db.Exec(ctx,
		`UPDATE invites SET used_at = now() WHERE id = $1 AND used_at IS NULL`, id,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// Delete revokes an invite; false when it was already gone.
func (r *InviteRepository) Delete(ctx context.Context, id uuid.UUID) (bool, error) {
	tag, err := r.db.Exec(ctx, `DELETE FROM invites WHERE id = $1`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
