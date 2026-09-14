// Package testkit provides test helpers: a Fiber app with the
// production middleware stack, and a live-Postgres harness for
// integration tests (migrations applied, schema truncated per test).
package testkit

import (
	"context"
	neturl "net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/glennprays/catet-backend/internal/middleware"
	"github.com/glennprays/log"
	"github.com/gofiber/fiber/v2"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5"
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

// NewDB connects to the *test* database, applies migrations, and
// truncates the schema after the test. Integration tests against a
// live Postgres are the honest gate for ledger behavior — no fake DB
// layer — but they must never be able to reach the dev data: the
// harness refuses any database whose name does not end in `_test`,
// and creates the test database when it is missing.
func NewDB(t *testing.T) *pgxpool.Pool {
	t.Helper()

	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		// Local-only dev default (matches .env.example); not a secret.
		url = "postgres://postgres:catet_dev@localhost:5432/catet_test?sslmode=disable" // #nosec G101
	}
	requireTestDatabase(t, url)
	ensureDatabase(t, url)

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

// requireTestDatabase is the guard that keeps `go test ./...` from
// wiping a working dataset: this harness truncates, so it only ever
// runs against a database named `*_test`.
func requireTestDatabase(t *testing.T, url string) {
	t.Helper()

	parsed, err := neturl.Parse(url)
	require.NoError(t, err, "parse TEST_DATABASE_URL")
	name := strings.TrimPrefix(parsed.Path, "/")
	if !strings.HasSuffix(name, "_test") {
		t.Fatalf(
			"refusing to run truncating tests against database %q: point TEST_DATABASE_URL at a *_test database",
			name,
		)
	}
}

// ensureDatabase creates the test database on first run so a fresh
// checkout needs no manual setup step.
func ensureDatabase(t *testing.T, url string) {
	t.Helper()

	parsed, err := neturl.Parse(url)
	require.NoError(t, err, "parse TEST_DATABASE_URL")
	name := strings.TrimPrefix(parsed.Path, "/")

	admin := *parsed
	admin.Path = "/postgres"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := pgx.Connect(ctx, admin.String())
	require.NoError(t, err, "connect to postgres (is it running? see make run-dev)")
	defer func() { _ = conn.Close(ctx) }()

	var exists bool
	err = conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, name).
		Scan(&exists)
	require.NoError(t, err, "look up test database")
	if exists {
		return
	}
	// Identifiers cannot be parameterized; the name is guarded above.
	_, err = conn.Exec(ctx, `CREATE DATABASE "`+name+`"`)
	require.NoError(t, err, "create test database")
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
