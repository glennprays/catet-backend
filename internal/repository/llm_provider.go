package repository

import (
	"context"
	"errors"

	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// LLMProviderRepository owns the one row of llm_provider: the AI
// provider the admin configured from the dashboard.
//
// It is the one repository in this package that never reads
// scope.UserID. The provider is instance-wide by design (one key, one
// budget, one bot), so the table has no user_id to scope by and "whose
// row is this" is not a question it can be asked.
type LLMProviderRepository struct {
	db *pgxpool.Pool
}

func NewLLMProviderRepository(db *pgxpool.Pool) *LLMProviderRepository {
	return &LLMProviderRepository{db: db}
}

// Get returns the configured provider, or nil when the dashboard has
// never saved one — the state in which the environment is authoritative.
func (r *LLMProviderRepository) Get(ctx context.Context) (*entity.LLMProvider, error) {
	var p entity.LLMProvider
	err := r.db.QueryRow(ctx, `
		SELECT base_url, api_key_enc, api_key_last4, model, vision_model,
		       timeout_seconds, vision_timeout_seconds, updated_by, updated_at
		FROM llm_provider
		WHERE singleton
	`).Scan(
		&p.BaseURL, &p.APIKeyEnc, &p.APIKeyLast4, &p.Model, &p.VisionModel,
		&p.TimeoutSeconds, &p.VisionTimeoutSeconds, &p.UpdatedBy, &p.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// Upsert writes the whole provider. The singleton primary key makes the
// conflict clause the only concurrency control needed: two admins
// saving at once produce one row, last write wins, and updated_at/by
// say who it was.
func (r *LLMProviderRepository) Upsert(ctx context.Context, p entity.LLMProvider) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO llm_provider (
			singleton, base_url, api_key_enc, api_key_last4, model, vision_model,
			timeout_seconds, vision_timeout_seconds, updated_by
		) VALUES (true, $1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (singleton) DO UPDATE SET
			base_url = EXCLUDED.base_url,
			api_key_enc = EXCLUDED.api_key_enc,
			api_key_last4 = EXCLUDED.api_key_last4,
			model = EXCLUDED.model,
			vision_model = EXCLUDED.vision_model,
			timeout_seconds = EXCLUDED.timeout_seconds,
			vision_timeout_seconds = EXCLUDED.vision_timeout_seconds,
			updated_by = EXCLUDED.updated_by,
			updated_at = now()
	`,
		p.BaseURL, p.APIKeyEnc, p.APIKeyLast4, p.Model, p.VisionModel,
		p.TimeoutSeconds, p.VisionTimeoutSeconds, p.UpdatedBy,
	)
	return err
}

// Delete drops the dashboard override, putting the instance back on the
// environment. Deleting a row that is not there is not an error: the
// desired state is "no override" either way.
func (r *LLMProviderRepository) Delete(ctx context.Context) error {
	_, err := r.db.Exec(ctx, `DELETE FROM llm_provider WHERE singleton`)
	return err
}
