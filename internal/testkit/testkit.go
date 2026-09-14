// Package testkit provides test helpers: a Fiber app with the
// production middleware stack, and a live-Postgres harness for
// integration tests (migrations applied, schema truncated per test).
package testkit

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/glennprays/catet-backend/internal/middleware"
	"github.com/glennprays/log"
	"github.com/gofiber/fiber/v2"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// NewTestApp returns a Fiber app wired with the production middleware
// stack (TraceID, ErrorHandler) and a quiet logger filtered to errors
// only. The register callback mounts whatever routes the test needs.
func NewTestApp(t *testing.T, register func(*fiber.App)) *fiber.App {
	t.Helper()

	logger, err := log.New(log.Config{
		Service: "test",
		Env:     "development",
		Level:   log.Level("error"),
		Output:  log.OutputType("stdout"),
	})
	if err != nil {
		t.Fatalf("create test logger: %v", err)
	}

	app := fiber.New(fiber.Config{
		ErrorHandler:          middleware.ErrorHandler(logger),
		DisableStartupMessage: true,
	})
	app.Use(middleware.TraceID())

	register(app)
	return app
}

// NewDB connects to DATABASE_URL (dev default), applies migrations,
// and truncates the schema after the test. Integration tests against
// a live Postgres are the honest gate for ledger behavior — no fake
// DB layer.
func NewDB(t *testing.T) *pgxpool.Pool {
	t.Helper()

	url := os.Getenv("DATABASE_URL")
	if url == "" {
		// Local-only dev default (matches .env.example); not a secret.
		url = "postgres://postgres:catet_dev@localhost:5432/catet_dev?sslmode=disable" // #nosec G101
	}

	pool, err := pgxpool.New(context.Background(), url)
	require.NoError(t, err, "connect to postgres (is it running? see make run-dev)")

	m, err := migrate.New("file://../../migrations", url)
	require.NoError(t, err, "open migrations")
	if err := m.Up(); err != nil && err.Error() != "no change" {
		t.Fatalf("apply migrations: %v", err)
	}

	t.Cleanup(func() {
		TruncateAll(t, pool)
		pool.Close()
	})
	return pool
}

// TruncateAll wipes domain tables between test cases.
func TruncateAll(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := pool.Exec(ctx,
		`TRUNCATE legs, transactions, categories, sources, bills, budgets,
		        pending_actions, otp_codes CASCADE`,
	)
	require.NoError(t, err, "truncate schema")
}
