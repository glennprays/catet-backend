package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/glennprays/catet-backend/internal/testkit"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newInbound(t *testing.T, poolPhone, messageID, body string) (context.Context, *repository.InboundMessageRepository, *repository.InboundMessage) {
	t.Helper()
	pool := testkit.NewDB(t)
	ctx, userID := testkit.Scoped(t, pool, poolPhone, "Inbound lifecycle")
	return ctx, repository.NewInboundMessageRepository(pool), &repository.InboundMessage{
		MessageID: messageID,
		Chat:      poolPhone + "@s.whatsapp.net",
		FromJID:   poolPhone + "@s.whatsapp.net",
		UserID:    &userID,
		OccurredAt: time.Now().UTC(),
		Body:       body,
		Kind:       "text",
	}
}

func claimWork(t *testing.T, ctx context.Context, repo *repository.InboundMessageRepository, m *repository.InboundMessage) *repository.InboundClaim {
	t.Helper()
	claim, err := repo.ClaimInbound(ctx, m)
	require.NoError(t, err)
	require.Equal(t, repository.InboundClaimWork, claim.Kind)
	require.NotNil(t, claim.Lease)
	return claim
}

func storeResult(t *testing.T, ctx context.Context, repo *repository.InboundMessageRepository, lease *repository.InboundLease, ids []uuid.UUID) *repository.InboundLease {
	t.Helper()
	lease, err := repo.StoreInboundResult(ctx, lease, repository.InboundResult{
		ReplyText:       "Sip, tercatat.",
		TransactionIDs:  ids,
		Lang:            "en",
		OutcomeReaction: "accepted",
	})
	require.NoError(t, err)
	require.NotNil(t, lease)
	return lease
}

// A regression here means ClaimInbound no longer atomically fences a first
// worker, allowing a duplicate webhook to perform the same ledger work.
func TestClaimInboundFencesDuplicateWorkAndStoresResultBeforeDelivery(t *testing.T) {
	pool := testkit.NewDB(t)
	ctx, userID := testkit.Scoped(t, pool, "+628111000801", "Inbound claim")
	repo := repository.NewInboundMessageRepository(pool)
	m := &repository.InboundMessage{
		MessageID: "wamid.claim", Chat: "628111000801@s.whatsapp.net", FromJID: "628111000801@s.whatsapp.net",
		UserID: &userID, OccurredAt: time.Now().UTC(), Body: "kopi 25k", Kind: "text",
	}

	work := claimWork(t, ctx, repo, m)
	busy, err := repo.ClaimInbound(ctx, m)
	require.NoError(t, err)
	assert.Equal(t, repository.InboundClaimBusy, busy.Kind)

	id := uuid.New()
	delivery := storeResult(t, ctx, repo, work.Lease, []uuid.UUID{id})
	stored, err := repo.Get(ctx, m.MessageID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, "ready", stored.Status)
	assert.Equal(t, "Sip, tercatat.", *stored.ReplyText)
	assert.Equal(t, []uuid.UUID{id}, stored.TransactionIDs)
	assert.Equal(t, "en", stored.Lang)
	require.NotNil(t, stored.OutcomeReaction)
	assert.Equal(t, "accepted", *stored.OutcomeReaction)

	require.ErrorIs(t, repo.MarkInboundReplied(ctx, work.Lease), repository.ErrInboundLeaseLost)
	_, err = repo.ReleaseInboundWork(ctx, work.Lease, []uuid.UUID{id}, "capture_write_timeout")
	require.ErrorIs(t, err, repository.ErrInboundLeaseLost)
	require.NoError(t, repo.MarkInboundReplied(ctx, delivery))
	replied, err := repo.Get(ctx, m.MessageID)
	require.NoError(t, err)
	assert.Equal(t, "replied", replied.Status)
	replay, err := repo.ClaimInbound(ctx, m)
	require.NoError(t, err)
	assert.Equal(t, repository.InboundClaimReplay, replay.Kind)
	assert.Equal(t, "replied", replay.Message.Status)
}

// A regression here either retries a held message too early, misses due work,
// or makes recovery order nondeterministic under a bounded startup drain.
func TestListDueInboundIsOldestFirstBoundedAndExcludesLeasedRows(t *testing.T) {
	pool := testkit.NewDB(t)
	ctx, userID := testkit.Scoped(t, pool, "+628111000802", "Inbound due")
	repo := repository.NewInboundMessageRepository(pool)
	makeMessage := func(id string) *repository.InboundMessage {
		return &repository.InboundMessage{MessageID: id, Chat: "628111000802@s.whatsapp.net", FromJID: "628111000802@s.whatsapp.net", UserID: &userID, OccurredAt: time.Now().UTC(), Body: id + " 1k", Kind: "text"}
	}
	first := claimWork(t, ctx, repo, makeMessage("wamid.due.first"))
	second := claimWork(t, ctx, repo, makeMessage("wamid.due.second"))
	leased := claimWork(t, ctx, repo, makeMessage("wamid.due.leased"))
	require.NotNil(t, leased.Lease)
	_, err := repo.ReleaseInboundWork(ctx, first.Lease, nil, "work_timeout")
	require.NoError(t, err)
	_, err = repo.ReleaseInboundWork(ctx, second.Lease, nil, "work_timeout")
	require.NoError(t, err)

	rows, err := repo.ListDueInbound(ctx, 10)
	require.NoError(t, err)
	assert.Empty(t, rows, "backoff and an active lease must both keep rows out of the due drain")

	_, err = pool.Exec(context.Background(), `UPDATE inbound_messages SET next_attempt_at = now() - interval '2 seconds' WHERE message_id = $1`, "wamid.due.first")
	require.NoError(t, err)
	_, err = pool.Exec(context.Background(), `UPDATE inbound_messages SET next_attempt_at = now() - interval '1 second' WHERE message_id = $1`, "wamid.due.second")
	require.NoError(t, err)
	rows, err = repo.ListDueInbound(ctx, 1)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "wamid.due.first", rows[0].MessageID)
	rows, err = repo.ListDueInbound(ctx, 10)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, []string{"wamid.due.first", "wamid.due.second"}, []string{rows[0].MessageID, rows[1].MessageID})
}

// A regression here can send sensitive transport failures into the audit
// column or let a stale delivery worker finalize a newer retry.
func TestRescheduleInboundSanitizesErrorsAndFencesStaleDeliveryLease(t *testing.T) {
	pool := testkit.NewDB(t)
	ctx, userID := testkit.Scoped(t, pool, "+628111000803", "Inbound retry")
	repo := repository.NewInboundMessageRepository(pool)
	m := &repository.InboundMessage{MessageID: "wamid.retry", Chat: "628111000803@s.whatsapp.net", FromJID: "628111000803@s.whatsapp.net", UserID: &userID, OccurredAt: time.Now().UTC(), Body: "kopi 25k", Kind: "text"}
	delivery := storeResult(t, ctx, repo, claimWork(t, ctx, repo, m).Lease, nil)

	started := time.Now()
	require.NoError(t, repo.RescheduleInbound(ctx, delivery, "gateway timeout https://secret.example/receipt?body=kopi"))
	stored, err := repo.Get(ctx, m.MessageID)
	require.NoError(t, err)
	require.NotNil(t, stored.LastError)
	assert.Equal(t, "internal_error", *stored.LastError)
	assert.Equal(t, 1, stored.DeliveryAttempts)
	assert.WithinDuration(t, started.Add(15*time.Second), stored.NextAttemptAt, 2*time.Second)
	require.ErrorIs(t, repo.MarkInboundReplied(ctx, delivery), repository.ErrInboundLeaseLost)

	_, err = pool.Exec(context.Background(), `UPDATE inbound_messages SET next_attempt_at = now() - interval '1 second' WHERE message_id = $1`, m.MessageID)
	require.NoError(t, err)
	retry, err := repo.ClaimInbound(ctx, m)
	require.NoError(t, err)
	require.NoError(t, repo.RescheduleInbound(ctx, retry.Lease, "reply_transport_timeout"))
	stored, err = repo.Get(ctx, m.MessageID)
	require.NoError(t, err)
	assert.Equal(t, "reply_transport_timeout", *stored.LastError)
	assert.WithinDuration(t, time.Now().Add(30*time.Second), stored.NextAttemptAt, 2*time.Second)
	for attempt := 3; attempt <= 6; attempt++ {
		_, err = pool.Exec(context.Background(), `UPDATE inbound_messages SET next_attempt_at = now() - interval '1 second' WHERE message_id = $1`, m.MessageID)
		require.NoError(t, err)
		retry, err = repo.ClaimInbound(ctx, m)
		require.NoError(t, err)
		require.Equal(t, repository.InboundClaimDelivery, retry.Kind)
		require.NoError(t, repo.RescheduleInbound(ctx, retry.Lease, "reply_transport_timeout"))
		stored, err = repo.Get(ctx, m.MessageID)
		require.NoError(t, err)
		assert.LessOrEqual(t, stored.NextAttemptAt.Sub(time.Now()), 5*time.Minute+2*time.Second)
	}
	assert.WithinDuration(t, time.Now().Add(5*time.Minute), stored.NextAttemptAt, 2*time.Second)
}

// A regression here loses the ids already written by a partial batch, or
// leaves the final failure without a lease through which a durable reply can
// be stored.
func TestReleaseInboundWorkPreservesPartialIDsAndLeavesFinalAttemptStorable(t *testing.T) {
	pool := testkit.NewDB(t)
	ctx, userID := testkit.Scoped(t, pool, "+628111000804", "Inbound work retry")
	repo := repository.NewInboundMessageRepository(pool)
	m := &repository.InboundMessage{MessageID: "wamid.partial", Chat: "628111000804@s.whatsapp.net", FromJID: "628111000804@s.whatsapp.net", UserID: &userID, OccurredAt: time.Now().UTC(), Body: "kopi 25k", Kind: "text"}
	partial := uuid.New()
	work := claimWork(t, ctx, repo, m)
	final, err := repo.ReleaseInboundWork(ctx, work.Lease, []uuid.UUID{partial}, "capture_write_timeout")
	require.NoError(t, err)
	assert.False(t, final)
	stored, err := repo.Get(ctx, m.MessageID)
	require.NoError(t, err)
	assert.Equal(t, []uuid.UUID{partial}, stored.TransactionIDs)
	assert.Equal(t, "processing", stored.Status)

	for attempt := 2; attempt <= 3; attempt++ {
		_, err = pool.Exec(context.Background(), `UPDATE inbound_messages SET next_attempt_at = now() - interval '1 second' WHERE message_id = $1`, m.MessageID)
		require.NoError(t, err)
		work = claimWork(t, ctx, repo, m)
		final, err = repo.ReleaseInboundWork(ctx, work.Lease, []uuid.UUID{partial}, "capture_write_timeout")
		require.NoError(t, err)
		if attempt < 3 {
			assert.False(t, final)
		} else {
			assert.True(t, final)
			delivery := storeResult(t, ctx, repo, work.Lease, []uuid.UUID{partial})
			require.NoError(t, repo.MarkInboundReplied(ctx, delivery))
		}
	}
}

// A regression here reruns an older stalled text after an equivalent later
// delivery has already recorded the money, while suppressing terminal rows
// would reject a legitimate second identical purchase.
func TestSupersedeEquivalentTextOnlyStopsStalledRows(t *testing.T) {
	pool := testkit.NewDB(t)
	ctx, userID := testkit.Scoped(t, pool, "+628111000805", "Inbound supersede")
	repo := repository.NewInboundMessageRepository(pool)
	makeMessage := func(id, body string) *repository.InboundMessage {
		return &repository.InboundMessage{MessageID: id, Chat: "628111000805@s.whatsapp.net", FromJID: "628111000805@s.whatsapp.net", UserID: &userID, OccurredAt: time.Now().UTC(), Body: body, Kind: "text"}
	}
	old := makeMessage("wamid.stalled", "Kopi   25k")
	oldWork := claimWork(t, ctx, repo, old)
	_, err := repo.ReleaseInboundWork(ctx, oldWork.Lease, nil, "work_timeout")
	require.NoError(t, err)
	newer := makeMessage("wamid.replied", "kopi 25k")
	newerDelivery := storeResult(t, ctx, repo, claimWork(t, ctx, repo, newer).Lease, nil)
	require.NoError(t, repo.MarkInboundReplied(ctx, newerDelivery))

	superseded, err := repo.SupersedeEquivalentText(ctx, old.MessageID)
	require.NoError(t, err)
	assert.True(t, superseded)
	stalled, err := repo.Get(ctx, old.MessageID)
	require.NoError(t, err)
	assert.Equal(t, "superseded", stalled.Status)

	second := makeMessage("wamid.replied-again", "KOPI\n25k")
	secondDelivery := storeResult(t, ctx, repo, claimWork(t, ctx, repo, second).Lease, nil)
	require.NoError(t, repo.MarkInboundReplied(ctx, secondDelivery))
	superseded, err = repo.SupersedeEquivalentText(ctx, newer.MessageID)
	require.NoError(t, err)
	assert.False(t, superseded)
	again, err := repo.Get(ctx, newer.MessageID)
	require.NoError(t, err)
	assert.Equal(t, "replied", again.Status)
}

func TestTerminalInboundRowsAreNeverClaimed(t *testing.T) {
	ctx, repo, m := newInbound(t, "+628111000806", "wamid.dropped", "ignored")
	m.Status = "dropped"
	claim, err := repo.ClaimInbound(ctx, m)
	require.NoError(t, err)
	assert.Equal(t, repository.InboundClaimReplay, claim.Kind)
	assert.Equal(t, "dropped", claim.Message.Status)
	require.Nil(t, claim.Lease, "terminal claims never carry a lease")
}
