// Command catet is the operator CLI: the break-glass path for account
// work that has no place in the dashboard. It talks to the same
// database and the same services the API does, so an invite issued here
// is indistinguishable from one issued from Settings → Pengguna.
//
// In the container image it lives next to the API binary and the final
// stage is scratch (no shell), so it is run as one process per command:
//
//	docker compose exec api /catet user list
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/glennprays/catet-backend/config"
	"github.com/glennprays/catet-backend/internal/auth"
	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/gateway"
	"github.com/glennprays/catet-backend/internal/infrastructure"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/glennprays/catet-backend/migrations"
	"github.com/glennprays/catet-backend/pkg/logger"
	"github.com/golang-migrate/migrate/v4"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const usageText = `catet — operator CLI

usage:
  catet user list
  catet user invite <phone>
  catet user set-phone <old-phone> <new-phone>
  catet user deactivate <phone>
  catet user activate <phone>
  catet user reset-password <phone>
  catet migrate up
  catet migrate down
  catet migrate version

Phones are accepted in any local form (0812…, 62812…, +62 812-…) and
stored normalised. Configuration comes from the environment, exactly as
the API reads it.`

// errUsage marks a wrong invocation: the caller gets the usage text and
// exit code 2, everything else exits 1 with just the message.
var errUsage = errors.New("usage")

// resetTTL matches the dashboard reset flow: long enough to paste the
// link into a browser, short enough that a leaked terminal scrollback
// stops being useful quickly.
const resetTTL = 15 * time.Minute

// tokenBytes mirrors internal/auth: 32 bytes of randomness behind a
// reset link, stored only as its sha256.
const tokenBytes = 32

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, errUsage) {
			fmt.Fprintln(os.Stderr, usageText)
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "catet: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) < 2 {
		return errUsage
	}
	family, command, rest := args[0], args[1], args[2:]

	// Migrations run before the pool exists: the schema they create is
	// what the repositories assume, and the init container that calls
	// this has nothing else wired.
	if family == "migrate" {
		if len(rest) != 0 {
			return errUsage
		}
		return runMigrate(command)
	}
	if family != "user" {
		return errUsage
	}

	// Argument shape is checked before anything connects: a typo should
	// not need a reachable database to be reported.
	var want int
	switch command {
	case "list":
		want = 0
	case "invite", "deactivate", "activate", "reset-password":
		want = 1
	case "set-phone":
		want = 2
	default:
		return errUsage
	}
	if len(rest) != want {
		return errUsage
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cli, err := newCLI()
	if err != nil {
		return err
	}
	defer cli.pool.Close()

	switch command {
	case "list":
		return cli.list(ctx)
	case "invite":
		return cli.invite(ctx, rest[0])
	case "set-phone":
		return cli.setPhone(ctx, rest[0], rest[1])
	case "deactivate":
		return cli.setActive(ctx, rest[0], false)
	case "activate":
		return cli.setActive(ctx, rest[0], true)
	case "reset-password":
		return cli.resetPassword(ctx, rest[0])
	}
	return errUsage
}

// runMigrate applies or inspects the embedded schema. It loads config
// only for DATABASE_URL, so the init container that runs it needs
// exactly the environment the API already has.
func runMigrate(command string) error {
	switch command {
	case "up", "down", "version":
	default:
		return errUsage
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	m, err := migrations.NewMigrator(cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("open migrations: %w", err)
	}
	defer m.Close()

	switch command {
	case "up":
		if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			return fmt.Errorf("migrate up: %w", err)
		}
	case "down":
		// One step, deliberately: a full down would drop the ledger.
		if err := m.Steps(-1); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			return fmt.Errorf("migrate down: %w", err)
		}
	}

	version, dirty, err := m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		fmt.Println("none")
		return nil
	}
	if err != nil {
		return fmt.Errorf("read version: %w", err)
	}
	if command == "version" {
		fmt.Printf("%d dirty=%t\n", version, dirty)
		return nil
	}
	fmt.Printf("migrated to version %d (dirty=%t)\n", version, dirty)
	return nil
}

// cli holds the same dependency graph internal/infrastructure wires for
// the API, minus everything that only serves HTTP.
type cli struct {
	cfg     *config.Config
	pool    *pgxpool.Pool
	gw      gateway.Sender
	users   *repository.UserRepository
	resets  *repository.PasswordResetRepository
	invites *auth.InviteService
}

func newCLI() (*cli, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	log, err := logger.ProviderLogger(cfg)
	if err != nil {
		return nil, err
	}
	pool, err := infrastructure.ProvidePool(cfg)
	if err != nil {
		return nil, err
	}
	gw := gateway.Provide(cfg, log)
	users := repository.NewUserRepository(pool)
	invites := repository.NewInviteRepository(pool)
	resets := repository.NewPasswordResetRepository(pool)
	return &cli{
		cfg:     cfg,
		pool:    pool,
		gw:      gw,
		users:   users,
		resets:  resets,
		invites: auth.NewInviteService(invites, users, gw, log, cfg.DashboardURL),
	}, nil
}

func (c *cli) list(ctx context.Context) error {
	users, err := c.users.List(ctx)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PHONE\tNAME\tADMIN\tSTATUS\tLAST LOGIN")
	for _, u := range users {
		status := "active"
		if u.DeactivatedAt != nil {
			status = "deactivated"
		}
		lastLogin := "-"
		if u.LastLoginAt != nil {
			lastLogin = u.LastLoginAt.Format(time.RFC3339)
		}
		fmt.Fprintf(w, "%s\t%s\t%t\t%s\t%s\n", u.Phone, u.DisplayName, u.IsAdmin, status, lastLogin)
	}
	return w.Flush()
}

func (c *cli) invite(ctx context.Context, phone string) error {
	admin, err := c.firstAdmin(ctx)
	if err != nil {
		return err
	}
	inv, token, err := c.invites.Issue(ctx, phone, admin.ID)
	if err != nil {
		return err
	}
	// The only place the raw link is ever printed. The invite normally
	// travels over WhatsApp; when the gateway is down that send fails
	// silently by design, and this line is how the operator hands the
	// link over by some other means.
	fmt.Printf("invite sent to %s; link: %s\n", inv.Phone, c.invites.RegisterURL(token))
	return nil
}

func (c *cli) setPhone(ctx context.Context, oldPhone, newPhone string) error {
	oldNormalized, err := auth.NormalizePhone(oldPhone)
	if err != nil {
		return err
	}
	newNormalized, err := auth.NormalizePhone(newPhone)
	if err != nil {
		return err
	}
	u, err := c.mustUser(ctx, oldNormalized)
	if err != nil {
		return err
	}
	existing, err := c.users.GetByPhone(ctx, newNormalized)
	if err != nil {
		return err
	}
	// Moving an account onto its own number is the documented way to
	// re-resolve a stale WhatsApp lid, so only a *different* account
	// holding the number is a conflict.
	if existing != nil && existing.ID != u.ID {
		return fmt.Errorf("%s already belongs to another account", newNormalized)
	}

	// A missing lid is not an error: the gateway may not know the
	// number yet, and the bot learns it on the first inbound message.
	var lidPtr *string
	lid, err := c.gw.ContactLID(ctx, newNormalized)
	if err == nil && lid != "" {
		lidPtr = &lid
	}
	if err := c.users.SetPhone(ctx, u.ID, newNormalized, lidPtr); err != nil {
		return err
	}
	if err := c.users.BumpEpoch(ctx, u.ID); err != nil {
		return err
	}

	shown := "none"
	if lidPtr != nil {
		shown = *lidPtr
	}
	fmt.Printf("phone updated; lid=%s; all sessions revoked\n", shown)
	return nil
}

func (c *cli) setActive(ctx context.Context, phone string, active bool) error {
	normalized, err := auth.NormalizePhone(phone)
	if err != nil {
		return err
	}
	u, err := c.mustUser(ctx, normalized)
	if err != nil {
		return err
	}

	var at *time.Time
	if !active {
		// Locking out the last admin would leave nobody able to invite,
		// deactivate or see the user list from the dashboard.
		if u.IsAdmin {
			if err := c.adminWouldRemain(ctx, u.ID); err != nil {
				return err
			}
		}
		now := time.Now()
		at = &now
	}
	if err := c.users.SetDeactivated(ctx, u.ID, at); err != nil {
		return err
	}
	if active {
		fmt.Printf("%s activated\n", u.Phone)
	} else {
		fmt.Printf("%s deactivated; all sessions revoked\n", u.Phone)
	}
	return nil
}

func (c *cli) resetPassword(ctx context.Context, phone string) error {
	normalized, err := auth.NormalizePhone(phone)
	if err != nil {
		return err
	}
	u, err := c.mustUser(ctx, normalized)
	if err != nil {
		return err
	}

	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Errorf("generate token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(buf)
	if err := c.resets.Create(ctx, u.ID, auth.HashToken(token), time.Now().Add(resetTTL)); err != nil {
		return err
	}
	fmt.Printf("%s/reset?token=%s\n", c.cfg.DashboardURL, token)
	return nil
}

// mustUser resolves a normalised phone to an account, turning "no such
// number" into a message the operator can act on.
func (c *cli) mustUser(ctx context.Context, normalized string) (*entity.User, error) {
	u, err := c.users.GetByPhone(ctx, normalized)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, fmt.Errorf("no account with phone %s", normalized)
	}
	return u, nil
}

// firstAdmin is the invite's author. Invites are admin-created by
// definition, and there is exactly one admin in practice.
func (c *cli) firstAdmin(ctx context.Context) (*entity.User, error) {
	users, err := c.users.List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range users {
		if users[i].IsAdmin {
			return &users[i], nil
		}
	}
	return nil, errors.New("no admin account exists")
}

// adminWouldRemain refuses a deactivation that would leave the install
// without anyone who can administer it.
func (c *cli) adminWouldRemain(ctx context.Context, deactivating uuid.UUID) error {
	users, err := c.users.List(ctx)
	if err != nil {
		return err
	}
	for _, u := range users {
		if u.ID == deactivating || !u.IsAdmin || u.DeactivatedAt != nil {
			continue
		}
		return nil
	}
	return errors.New("no active admin would remain")
}
