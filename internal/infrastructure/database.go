// Package infrastructure hosts the long-lived runtime dependencies
// (config, logger, database pool) and the Wire injector that wires
// the whole application graph together.
package infrastructure

import (
	"context"
	"fmt"
	"time"

	"github.com/glennprays/catet-backend/config"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NewPool opens the pgx connection pool used by every repository.
// The pool stays small on purpose: the API serves a handful of invited
// users, so ten connections cover concurrent request handling — HTTP
// plus the bot's own goroutines — without holding idle Postgres
// backends on a self-hosted box.
func NewPool(ctx context.Context, cfg *config.Config) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	poolCfg.MaxConns = 10
	poolCfg.MinConns = 0
	poolCfg.MaxConnLifetime = time.Hour
	poolCfg.MaxConnIdleTime = 15 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	return pool, nil
}
