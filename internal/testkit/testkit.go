// Package testkit provides test helpers: a Fiber app with the
// production middleware stack, and a live-Postgres harness for
// integration tests (migrations applied, schema truncated per test).
package testkit

import (
	"context"
	"errors"
	neturl "net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/glennprays/catet-backend/internal/middleware"
	"github.com/glennprays/catet-backend/internal/scope"
	"github.com/glennprays/catet-backend/migrations"
	"github.com/glennprays/log"
	"github.com/gofiber/fiber/v2"
	"github.com/golang-migrate/migrate/v4"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// Logger is the quiet test logger: errors only, so a passing test is
// silent and a failing one still says why.
func Logger(t *testing.T) *log.Logger {
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
	return logger
}

// NewTestApp returns a Fiber app wired with the production middleware
// stack (TraceID, ErrorHandler) and a quiet logger filtered to errors
// only. The register callback mounts whatever routes the test needs.
func NewTestApp(t *testing.T, register func(*fiber.App)) *fiber.App {
	t.Helper()

	logger := Logger(t)

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
//
// Each package gets its own database. `go test ./...` runs packages
// concurrently and this harness truncates, so one shared database would
// let the bot suite wipe the ledger suite's rows mid-test.
func NewDB(t *testing.T) *pgxpool.Pool {
	t.Helper()

	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		// Local-only dev default (matches .env.example); not a secret.
		url = "postgres://postgres:catet_dev@localhost:5432/catet_test?sslmode=disable" // #nosec G101
	}
	url = perPackageDatabase(t, url, callerPackage())
	requireTestDatabase(t, url)
	ensureDatabase(t, url)

	pool, err := pgxpool.New(context.Background(), url)
	require.NoError(t, err, "connect to postgres (is it running? see make run-dev)")

	// Same embedded source the production binary migrates with, so a
	// migration that only exists on disk can never pass tests.
	m, err := migrations.NewMigrator(url)
	require.NoError(t, err, "open migrations")
	defer m.Close()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("apply migrations: %v", err)
	}

	t.Cleanup(func() {
		TruncateAll(t, pool)
		pool.Close()
	})
	return pool
}

// callerPackage names the package under test from the directory of the
// test file that asked for a database.
func callerPackage() string {
	// 0 = this frame, 1 = NewDB, 2 = the calling test file.
	_, file, _, ok := runtime.Caller(2)
	if !ok {
		return ""
	}
	return sanitizeIdentifier(filepath.Base(filepath.Dir(file)))
}

// sanitizeIdentifier keeps only what is safe in an unquoted SQL name.
func sanitizeIdentifier(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// perPackageDatabase rewrites the database name to "<base>_<pkg>_test",
// preserving the `_test` suffix the guard below requires.
func perPackageDatabase(t *testing.T, url, pkg string) string {
	t.Helper()
	if pkg == "" {
		return url
	}
	parsed, err := neturl.Parse(url)
	require.NoError(t, err, "parse TEST_DATABASE_URL")
	name := strings.TrimPrefix(parsed.Path, "/")
	if !strings.HasSuffix(name, "_test") {
		// Left as is: the guard below rejects it with a better message.
		return url
	}
	parsed.Path = "/" + strings.TrimSuffix(name, "_test") + "_" + pkg + "_test"
	return parsed.String()
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

// TruncateAll wipes domain tables between test cases. Users go too: a
// test's tenants are part of its fixture, so the next test starts with
// no accounts at all (not even the migration's bootstrap admin).
func TruncateAll(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := pool.Exec(ctx,
		`TRUNCATE legs, transactions, categories, sources, bills, budgets, tags,
		        bot_chat_state, inbound_messages, pending_actions, otp_codes,
		        password_resets, invites, user_settings, users CASCADE`,
	)
	require.NoError(t, err, "truncate schema")
}

// CreateUser inserts a passwordless account plus its settings row and
// returns its id — the tenant every scoped fixture writes under.
func CreateUser(t *testing.T, pool *pgxpool.Pool, phone, name string) uuid.UUID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var id uuid.UUID
	err := pool.QueryRow(ctx,
		`INSERT INTO users (phone, display_name) VALUES ($1, $2) RETURNING id`,
		phone, name,
	).Scan(&id)
	require.NoError(t, err, "create test user")

	_, err = pool.Exec(ctx, `INSERT INTO user_settings (user_id) VALUES ($1)`, id)
	require.NoError(t, err, "create test user settings")
	return id
}

// Scoped is CreateUser plus the context every repository reads its
// tenant from.
func Scoped(t *testing.T, pool *pgxpool.Pool, phone, name string) (context.Context, uuid.UUID) {
	t.Helper()
	id := CreateUser(t, pool, phone, name)
	return scope.WithUserID(context.Background(), id), id
}
