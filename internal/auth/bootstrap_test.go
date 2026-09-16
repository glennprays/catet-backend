package auth

import (
	"testing"

	"github.com/glennprays/catet-backend/config"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/glennprays/catet-backend/internal/testkit"
	"github.com/glennprays/log"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bootstrapID is the id migration 0006 gives the placeholder admin.
const bootstrapID = "00000000-0000-0000-0000-000000000001"

// insertBootstrapAdmin re-creates what the migration inserts.
// TruncateAll wipes users, so the unclaimed state is a fixture here
// rather than something the schema hands us.
func insertBootstrapAdmin(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(t.Context(),
		`INSERT INTO users (id, phone, display_name, is_admin) VALUES ($1, $2, 'Admin', true)`,
		bootstrapID, BootstrapPhone,
	)
	require.NoError(t, err, "insert bootstrap admin")
	_, err = pool.Exec(t.Context(), `INSERT INTO user_settings (user_id) VALUES ($1)`, bootstrapID)
	require.NoError(t, err, "insert bootstrap settings")
}

func quietLogger(t *testing.T) *log.Logger {
	t.Helper()
	logger, err := log.New(log.Config{
		Service: "test",
		Env:     "development",
		Level:   log.Level("error"),
		Output:  log.OutputType("stdout"),
	})
	require.NoError(t, err, "create test logger")
	return logger
}

// An install whose admin is still the placeholder cannot be logged into
// by anybody, so booting without OWNER_PHONE would produce a running
// API that nobody can ever reach. It has to fail loudly instead.
func TestClaimBootstrapAdmin_UnclaimedWithoutOwnerPhoneFails(t *testing.T) {
	pool := testkit.NewDB(t)
	insertBootstrapAdmin(t, pool)
	users := repository.NewUserRepository(pool)

	err := ClaimBootstrapAdmin(t.Context(), users, &fakeGateway{}, &config.Config{}, quietLogger(t))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "OWNER_PHONE", "the message must name the variable that fixes it")

	admin, err := users.GetByPhone(t.Context(), BootstrapPhone)
	require.NoError(t, err)
	assert.NotNil(t, admin, "a failed claim must leave the placeholder claimable")
}

// The claim is the whole of "how a fresh install gets its first user":
// the placeholder takes on the operator's real number, and the lid the
// gateway knows so the bot recognises their first message.
func TestClaimBootstrapAdmin_ClaimsThePlaceholder(t *testing.T) {
	pool := testkit.NewDB(t)
	insertBootstrapAdmin(t, pool)
	users := repository.NewUserRepository(pool)

	cfg := &config.Config{OwnerPhone: "08123456789"}
	require.NoError(t, ClaimBootstrapAdmin(
		t.Context(), users, &fakeGateway{lid: "55"}, cfg, quietLogger(t),
	))

	claimed, err := users.GetByPhone(t.Context(), "+628123456789")
	require.NoError(t, err)
	require.NotNil(t, claimed, "OWNER_PHONE is normalised before it is stored")
	assert.Equal(t, bootstrapID, claimed.ID.String(), "the existing admin is claimed, not replaced")
	assert.True(t, claimed.IsAdmin)
	require.NotNil(t, claimed.WhatsAppLID)
	assert.Equal(t, "55", *claimed.WhatsAppLID)

	gone, err := users.GetByPhone(t.Context(), BootstrapPhone)
	require.NoError(t, err)
	assert.Nil(t, gone, "the placeholder number is spent")
}

// An explicit OWNER_LID beats the gateway: the operator sets it exactly
// for the case where the gateway cannot resolve the number.
func TestClaimBootstrapAdmin_ExplicitLidWins(t *testing.T) {
	pool := testkit.NewDB(t)
	insertBootstrapAdmin(t, pool)
	users := repository.NewUserRepository(pool)

	cfg := &config.Config{OwnerPhone: "+628123456789", OwnerLid: "77@lid"}
	require.NoError(t, ClaimBootstrapAdmin(
		t.Context(), users, &fakeGateway{lid: "55"}, cfg, quietLogger(t),
	))

	claimed, err := users.GetByPhone(t.Context(), "+628123456789")
	require.NoError(t, err)
	require.NotNil(t, claimed.WhatsAppLID)
	assert.Equal(t, "77", *claimed.WhatsAppLID, "the lid is stored as digits only")
}

// After the one-time claim the env var is inert: leaving OWNER_PHONE in
// place must never rewrite an account's identity on a later boot.
func TestClaimBootstrapAdmin_AlreadyClaimedIsANoOp(t *testing.T) {
	pool := testkit.NewDB(t)
	users := repository.NewUserRepository(pool)
	existing := testkit.CreateUser(t, pool, "+628123456700", "Owner")

	cfg := &config.Config{OwnerPhone: "+628123456789"}
	require.NoError(t, ClaimBootstrapAdmin(
		t.Context(), users, &fakeGateway{lid: "55"}, cfg, quietLogger(t),
	))

	all, err := users.List(t.Context())
	require.NoError(t, err)
	require.Len(t, all, 1, "no account is created by a boot with nothing to claim")
	assert.Equal(t, existing, all[0].ID)
	assert.Equal(t, "+628123456700", all[0].Phone)
	assert.Nil(t, all[0].WhatsAppLID)
}
