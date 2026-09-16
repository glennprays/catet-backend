package repository

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PasswordResetRepository persists single-use reset tokens. The token
// is only ever stored hashed; the plaintext lives in the browser (or in
// the operator's terminal) and nowhere else.
type PasswordResetRepository struct {
	db *pgxpool.Pool
}

func NewPasswordResetRepository(db *pgxpool.Pool) *PasswordResetRepository {
	return &PasswordResetRepository{db: db}
}

func (r *PasswordResetRepository) Create(
	ctx context.Context, userID uuid.UUID, tokenHash string, expiresAt time.Time,
) error {
	_, err := r.db.Exec(ctx,
		`INSERT INTO password_resets (user_id, token_hash, expires_at) VALUES ($1,$2,$3)`,
		userID, tokenHash, expiresAt,
	)
	return err
}

// Consume claims a reset token exactly once. The UPDATE ... RETURNING is
// the whole atomicity story: two concurrent completions cannot both
// win, because only one of them matches `used_at IS NULL`. A nil user
// id means the token was unknown, spent or expired.
func (r *PasswordResetRepository) Consume(ctx context.Context, tokenHash string) (*uuid.UUID, error) {
	var userID uuid.UUID
	err := r.db.QueryRow(ctx,
		`UPDATE password_resets SET used_at = now()
		 WHERE token_hash = $1 AND used_at IS NULL AND expires_at > now()
		 RETURNING user_id`,
		tokenHash,
	).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &userID, nil
}
