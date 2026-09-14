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

// OtpRepository stores hashed OTP codes in Postgres.
type OtpRepository struct {
	db *pgxpool.Pool
}

func NewOtpRepository(db *pgxpool.Pool) *OtpRepository {
	return &OtpRepository{db: db}
}

// Create inserts a new hashed code for the phone.
func (r *OtpRepository) Create(ctx context.Context, phone, codeHash string, expiresAt time.Time) error {
	_, err := r.db.Exec(ctx,
		`INSERT INTO otp_codes (phone, code_hash, expires_at) VALUES ($1, $2, $3)`,
		phone, codeHash, expiresAt,
	)
	return err
}

// Latest returns the most recent code for the phone, or nil when none
// exists.
func (r *OtpRepository) Latest(ctx context.Context, phone string) (*entity.OtpCode, error) {
	row := r.db.QueryRow(ctx,
		`SELECT id, phone, code_hash, expires_at, attempts, consumed, created_at
		 FROM otp_codes WHERE phone = $1 ORDER BY created_at DESC LIMIT 1`,
		phone,
	)
	var c entity.OtpCode
	err := row.Scan(&c.ID, &c.Phone, &c.CodeHash, &c.ExpiresAt, &c.Attempts, &c.Consumed, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// IncrementAttempts bumps the failed-attempt counter and returns the
// new value.
func (r *OtpRepository) IncrementAttempts(ctx context.Context, id uuid.UUID) (int, error) {
	var attempts int
	err := r.db.QueryRow(ctx,
		`UPDATE otp_codes SET attempts = attempts + 1 WHERE id = $1 RETURNING attempts`,
		id,
	).Scan(&attempts)
	return attempts, err
}

// Consume marks a code as used (or invalidated) so it can never verify
// again.
func (r *OtpRepository) Consume(ctx context.Context, id uuid.UUID) error {
	_, err := r.db.Exec(ctx, `UPDATE otp_codes SET consumed = true WHERE id = $1`, id)
	return err
}
