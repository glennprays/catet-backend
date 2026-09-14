package infrastructure

import (
	"context"

	"github.com/glennprays/catet-backend/config"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/glennprays/catet-backend/internal/service"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ProvidePool opens the pgx pool for the process lifetime. Live in a
// non-wireinject file so both the injector and the hand-maintained
// wire_gen.go see it.
func ProvidePool(cfg *config.Config) (*pgxpool.Pool, error) {
	return NewPool(context.Background(), cfg)
}

// provideSettingsService injects the env-only owner phone.
func provideSettingsService(repo *repository.SettingsRepository, cfg *config.Config) *service.SettingsService {
	return service.NewSettingsService(repo, cfg.OwnerPhone)
}
