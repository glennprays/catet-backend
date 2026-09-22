package migrations_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/glennprays/catet-backend/internal/testkit"
	"github.com/glennprays/catet-backend/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
)

// TestInboundDeliveryMigration protects the inbox/outbox storage contract.
// In particular, removing the ready/superseded states, retry fields, or the
// due-work access path would make recovery either resend ledger work or leave
// a committed reply stranded.
func TestInboundDeliveryMigration(t *testing.T) {
	pool := testkit.NewDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	const readyMessageID = "inbound-delivery-ready"
	insertLegacyInbound(t, ctx, pool, readyMessageID, "ready")

	var (
		workAttempts     int
		deliveryAttempts int
		nextAttemptAt    time.Time
		leaseUntil       *time.Time
		lastError        *string
		outcomeReaction  *string
	)
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT work_attempts, delivery_attempts, next_attempt_at,
		       lease_until, last_error, outcome_reaction
		FROM inbound_messages
		WHERE message_id = $1
	`, readyMessageID).Scan(
		&workAttempts,
		&deliveryAttempts,
		&nextAttemptAt,
		&leaseUntil,
		&lastError,
		&outcomeReaction,
	))
	require.Zero(t, workAttempts)
	require.Zero(t, deliveryAttempts)
	require.WithinDuration(t, now, nextAttemptAt, 2*time.Second)
	require.Nil(t, leaseUntil)
	require.Nil(t, lastError)
	require.Nil(t, outcomeReaction)

	oldestDue := now.Add(-2 * time.Hour)
	laterDue := now.Add(-time.Hour)
	future := now.Add(time.Hour)
	require.NoError(t, updateNextAttemptAt(ctx, pool, readyMessageID, future))
	insertInbound(t, ctx, pool, "inbound-delivery-processing-due", "processing", &oldestDue)
	insertInbound(t, ctx, pool, "inbound-delivery-ready-due", "ready", &laterDue)
	insertInbound(t, ctx, pool, "inbound-delivery-replied", "replied", &oldestDue)
	insertInbound(t, ctx, pool, "inbound-delivery-superseded", "superseded", &oldestDue)

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `SET LOCAL enable_seqscan = off`)
	require.NoError(t, err)

	planRows, err := tx.Query(ctx, `
		EXPLAIN (COSTS OFF)
		SELECT message_id
		FROM inbound_messages
		WHERE status IN ('processing', 'ready')
		  AND next_attempt_at <= $1
		ORDER BY next_attempt_at, received_at
	`, now)
	require.NoError(t, err)
	plan, err := pgx.CollectRows(planRows, pgx.RowTo[string])
	require.NoError(t, err)
	require.Contains(t, strings.Join(plan, "\n"), "idx_inbound_due_work")
	require.NoError(t, tx.Rollback(ctx))

	rows, err := pool.Query(ctx, `
		SELECT message_id
		FROM inbound_messages
		WHERE status IN ('processing', 'ready')
		  AND next_attempt_at <= $1
		ORDER BY next_attempt_at, received_at
	`, now)
	require.NoError(t, err)
	dueMessages, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	require.Equal(t, []string{
		"inbound-delivery-processing-due",
		"inbound-delivery-ready-due",
	}, dueMessages)

	m, err := migrations.NewMigrator(pool.Config().ConnString())
	require.NoError(t, err)
	defer func() {
		_, _ = m.Close()
	}()
	require.NoError(t, m.Steps(-1))

	assertInboundStatus(t, ctx, pool, readyMessageID, "processing")
	assertInboundStatus(t, ctx, pool, "inbound-delivery-superseded", "dropped")
	insertLegacyInbound(t, ctx, pool, "inbound-delivery-legacy-processing", "processing")

	require.NoError(t, m.Steps(1))
	assertInboundStatus(t, ctx, pool, readyMessageID, "processing")
	assertInboundStatus(t, ctx, pool, "inbound-delivery-superseded", "dropped")

	var immediatelyDue bool
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT next_attempt_at <= now()
		FROM inbound_messages
		WHERE message_id = 'inbound-delivery-legacy-processing'
	`).Scan(&immediatelyDue))
	require.True(t, immediatelyDue)
}

func insertLegacyInbound(t *testing.T, ctx context.Context, pool interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, messageID, status string) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO inbound_messages (
			message_id, chat, from_jid, occurred_at, body, kind, status
		) VALUES ($1, '628111000000@s.whatsapp.net', '628111000000@s.whatsapp.net', now(), 'test', 'text', $2)
	`, messageID, status)
	require.NoError(t, err)
}

func insertInbound(t *testing.T, ctx context.Context, pool interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, messageID, status string, nextAttemptAt *time.Time) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO inbound_messages (
			message_id, chat, from_jid, occurred_at, body, kind, status, next_attempt_at
		) VALUES ($1, '628111000000@s.whatsapp.net', '628111000000@s.whatsapp.net', now(), 'test', 'text', $2, $3)
	`, messageID, status, nextAttemptAt)
	require.NoError(t, err)
}

func updateNextAttemptAt(ctx context.Context, pool interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, messageID string, nextAttemptAt time.Time) error {
	_, err := pool.Exec(ctx, `
		UPDATE inbound_messages
		SET next_attempt_at = $2
		WHERE message_id = $1
	`, messageID, nextAttemptAt)
	return err
}

func assertInboundStatus(t *testing.T, ctx context.Context, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, messageID, want string) {
	t.Helper()
	var got string
	require.NoError(t, pool.QueryRow(ctx, `SELECT status FROM inbound_messages WHERE message_id = $1`, messageID).Scan(&got))
	require.Equal(t, want, got)
}

