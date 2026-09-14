package repository

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SettingsRepository reads/writes the singleton app_settings row.
type SettingsRepository struct {
	db *pgxpool.Pool
}

func NewSettingsRepository(db *pgxpool.Pool) *SettingsRepository {
	return &SettingsRepository{db: db}
}

// Get returns the income horizon. The row is seeded by the migration,
// so ErrNoRows means an un-migrated database.
func (r *SettingsRepository) Get(ctx context.Context) (int, error) {
	var days int
	err := r.db.QueryRow(ctx, `SELECT income_horizon_days FROM app_settings WHERE id = true`).Scan(&days)
	if err != nil {
		return 0, err
	}
	return days, nil
}

func (r *SettingsRepository) UpdateIncomeHorizon(ctx context.Context, days int) error {
	tag, err := r.db.Exec(ctx,
		`UPDATE app_settings SET income_horizon_days = $2 WHERE id = true`,
		days,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}
