package repository

import (
	"context"

	"github.com/glennprays/catet-backend/internal/scope"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SettingsRepository reads/writes one user's row in user_settings.
type SettingsRepository struct {
	db *pgxpool.Pool
}

func NewSettingsRepository(db *pgxpool.Pool) *SettingsRepository {
	return &SettingsRepository{db: db}
}

// Get returns the income horizon. The row is created with the user, so
// ErrNoRows means a broken install, not a new account.
func (r *SettingsRepository) Get(ctx context.Context) (int, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return 0, err
	}
	var days int
	err = r.db.QueryRow(ctx, `SELECT income_horizon_days FROM user_settings WHERE user_id = $1`, uid).Scan(&days)
	if err != nil {
		return 0, err
	}
	return days, nil
}

func (r *SettingsRepository) UpdateIncomeHorizon(ctx context.Context, days int) error {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return err
	}
	tag, err := r.db.Exec(ctx,
		`UPDATE user_settings SET income_horizon_days = $2 WHERE user_id = $1`,
		uid, days,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// GetPhotoDefaultSource returns the Source a receipt falls back to when
// nothing else names one. A NULL column is not an error: it means the
// bot should keep asking, so the caller gets a nil id.
func (r *SettingsRepository) GetPhotoDefaultSource(ctx context.Context) (*uuid.UUID, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, err
	}
	var id *uuid.UUID
	err = r.db.QueryRow(ctx, `SELECT photo_default_source_id FROM user_settings WHERE user_id = $1`, uid).Scan(&id)
	if err != nil {
		return nil, err
	}
	return id, nil
}

// SetPhotoDefaultSource stores the receipt fallback Source; a nil id
// clears it and puts the bot back to asking every time.
func (r *SettingsRepository) SetPhotoDefaultSource(ctx context.Context, id *uuid.UUID) error {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return err
	}
	tag, err := r.db.Exec(ctx,
		`UPDATE user_settings SET photo_default_source_id = $2 WHERE user_id = $1`,
		uid, id,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// GetReminderHour returns the Jakarta hour the daily bill digest goes
// out (ADR-0006). One hour per user: the cap on unsolicited messages is
// one a day, so there is nothing per-bill to tune here.
func (r *SettingsRepository) GetReminderHour(ctx context.Context) (int, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return 0, err
	}
	var hour int
	err = r.db.QueryRow(ctx, `SELECT reminder_hour FROM user_settings WHERE user_id = $1`, uid).Scan(&hour)
	if err != nil {
		return 0, err
	}
	return hour, nil
}

func (r *SettingsRepository) SetReminderHour(ctx context.Context, hour int) error {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return err
	}
	tag, err := r.db.Exec(ctx, `UPDATE user_settings SET reminder_hour = $2 WHERE user_id = $1`, uid, hour)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}
