package bot_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glennprays/catet-backend/config"
	"github.com/glennprays/catet-backend/internal/auth"
	"github.com/glennprays/catet-backend/internal/bot"
	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/llm"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/glennprays/catet-backend/internal/scope"
	"github.com/glennprays/catet-backend/internal/service"
	"github.com/glennprays/catet-backend/internal/testkit"
	"github.com/glennprays/log"
	waga "github.com/glennprays/whatsapp-gateway-sdk-go"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const ownerPhone = "+628123456789"

// sentMessage is one captured outbound reply.
type sentMessage struct {
	Chat           string
	Text           string
	IdempotencyKey string
}

// reaction is one emoji the harness put on an inbound message.
type reaction struct {
	MessageID string
	Emoji     string
}

// fakeSender records replies, reactions and typing toggles instead of
// calling the gateway. lid is what the gateway would answer for a
// contact lookup; "" means it knows no lid for that number.
type fakeSender struct {
	mu      sync.Mutex
	sent    []sentMessage
	reacts  []reaction
	typing  []bool
	lid     string
	sendErr error
}

func (f *fakeSender) SendText(_ context.Context, chat, text, key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return "", f.sendErr
	}
	f.sent = append(f.sent, sentMessage{Chat: chat, Text: text, IdempotencyKey: key})
	return uuid.NewString(), nil
}

func (f *fakeSender) setSendError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sendErr = err
}

func (f *fakeSender) React(_ context.Context, _, messageID, _, emoji string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reacts = append(f.reacts, reaction{MessageID: messageID, Emoji: emoji})
	return nil
}

func (f *fakeSender) SetTyping(_ context.Context, _ string, typing bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.typing = append(f.typing, typing)
	return nil
}

func (f *fakeSender) ContactLID(_ context.Context, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lid, nil
}

// reactionsOn lists, in order, the emoji this message collected.
func (f *fakeSender) reactionsOn(messageID string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []string{}
	for _, r := range f.reacts {
		if r.MessageID == messageID {
			out = append(out, r.Emoji)
		}
	}
	return out
}

func (f *fakeSender) all() []sentMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]sentMessage, len(f.sent))
	copy(out, f.sent)
	return out
}

func (f *fakeSender) last() string {
	all := f.all()
	if len(all) == 0 {
		return ""
	}
	return all[len(all)-1].Text
}

type botFixture struct {
	ctx       context.Context
	userID    uuid.UUID
	pool      *pgxpool.Pool
	harness   *bot.Harness
	sender    *fakeSender
	cfg       *config.Config
	sources   *repository.SourceRepository
	cats      *repository.CategoryRepository
	txRepo    *repository.TransactionRepository
	pending   *repository.PendingActionRepository
	inbound   *repository.InboundMessageRepository
	chats     *repository.BotChatStateRepository
	settings  *repository.SettingsRepository
	users     *repository.UserRepository
	invites   *repository.InviteRepository
	inviteSvc *auth.InviteService
	bills     *repository.BillRepository
	billPays  *repository.BillPaymentRepository
	logger    *log.Logger
	bca       *entity.Source
	gopay     *entity.Source
	cash      *entity.Source
	spay      *entity.Source
	bri       *entity.Source
	dana      *entity.Source
	makanan   *entity.Category
	transpor  *entity.Category
}

func newBotFixture(t *testing.T, llmClient *llm.Client) *botFixture {
	t.Helper()
	pool := testkit.NewDB(t)

	logger, err := log.New(log.Config{
		Service: "test", Env: "development",
		Level: log.Level("error"), Output: log.OutputType("stdout"),
	})
	require.NoError(t, err)

	sourceRepo := repository.NewSourceRepository(pool)
	categoryRepo := repository.NewCategoryRepository(pool)
	transactionRepo := repository.NewTransactionRepository(pool)
	pendingRepo := repository.NewPendingActionRepository(pool)
	inboundRepo := repository.NewInboundMessageRepository(pool)
	chatRepo := repository.NewBotChatStateRepository(pool)
	analyticsRepo := repository.NewAnalyticsRepository(pool)
	billRepo := repository.NewBillRepository(pool)
	billPaymentRepo := repository.NewBillPaymentRepository(pool)
	reminderSendRepo := repository.NewBillReminderSendRepository(pool)
	settingsRepo := repository.NewSettingsRepository(pool)
	userRepo := repository.NewUserRepository(pool)
	inviteRepo := repository.NewInviteRepository(pool)

	merchantSvc := service.NewMerchantService(repository.NewMerchantRepository(pool), llmClient, logger)
	txSvc := service.NewTransactionService(transactionRepo, sourceRepo, categoryRepo, merchantSvc, billPaymentRepo, billRepo, logger)
	billSvc := service.NewBillService(billRepo, billPaymentRepo, txSvc, sourceRepo)
	analyticsSvc := service.NewAnalyticsService(analyticsRepo, categoryRepo, sourceRepo, billRepo, settingsRepo)
	overviewSvc := service.NewOverviewService(sourceRepo, billRepo, settingsRepo, analyticsRepo, pendingRepo, txSvc)

	sender := &fakeSender{}
	inviteSvc := auth.NewInviteService(inviteRepo, userRepo, sender, logger, "http://localhost:3001")
	cfg := &config.Config{Env: config.DEV}
	harness := bot.NewHarness(
		cfg, logger, sender, llmClient,
		txSvc, merchantSvc, overviewSvc, analyticsSvc, billSvc,
		sourceRepo, categoryRepo, transactionRepo, pendingRepo,
		inboundRepo, chatRepo, analyticsRepo, settingsRepo,
		userRepo, billRepo, reminderSendRepo, inviteSvc,
	)

	// The owner is an account like any other now: the fixture's context
	// is that account's scope, so every seed below lands in its ledger.
	userID := testkit.CreateUser(t, pool, ownerPhone, "Owner")
	f := &botFixture{
		ctx: scope.WithUserID(context.Background(), userID), userID: userID,
		pool: pool, harness: harness, sender: sender, cfg: cfg,
		sources: sourceRepo, cats: categoryRepo, txRepo: transactionRepo,
		pending: pendingRepo, inbound: inboundRepo, chats: chatRepo, settings: settingsRepo,
		users: userRepo, invites: inviteRepo, inviteSvc: inviteSvc,
		bills: billRepo, billPays: billPaymentRepo, logger: logger,
	}
	f.bca = f.source(t, "BCA", "bank", false)
	f.gopay = f.source(t, "GoPay", "ewallet", false)
	f.cash = f.source(t, "Cash", "cash", false)
	f.spay = f.source(t, "SPayLater", "custom", true)
	f.bri = f.source(t, "BRI", "bank", false)
	f.dana = f.source(t, "DANA", "ewallet", false)
	f.makanan = f.category(t, "Makanan & Minuman")
	f.transpor = f.category(t, "Transportasi")
	return f
}

func (f *botFixture) source(t *testing.T, name, typ string, isDebt bool) *entity.Source {
	t.Helper()
	src, err := f.sources.Create(f.ctx, &entity.Source{
		ID: uuid.New(), Name: name, Type: typ, IsDebt: isDebt, Currency: "IDR",
	})
	require.NoError(t, err)
	return src
}

func (f *botFixture) category(t *testing.T, name string) *entity.Category {
	t.Helper()
	cat, err := f.cats.Create(f.ctx, &entity.Category{ID: uuid.New(), Name: name})
	require.NoError(t, err)
	return cat
}

// say delivers one owner text message through the harness.
func (f *botFixture) say(t *testing.T, messageID, text string) {
	t.Helper()
	f.sayAs(t, ownerPhone, messageID, text)
}

// sayAs delivers one text message from any number, addressed the way
// the gateway addresses a one-to-one chat.
func (f *botFixture) sayAs(t *testing.T, phone, messageID, text string) {
	t.Helper()
	f.deliver(t, &waga.IncomingWebhookPayload{
		Event:     waga.IncomingEventMessageMessageIncoming,
		Chat:      phone[1:] + "@s.whatsapp.net",
		From:      phone[1:] + ":42@s.whatsapp.net",
		MessageId: messageID,
		PushName:  "Owner",
		Timestamp: time.Now().Unix(),
		Text:      text,
		Type:      waga.IncomingMessageTypeText,
	})
}

func (f *botFixture) deliver(t *testing.T, p *waga.IncomingWebhookPayload) {
	t.Helper()
	require.NoError(t, f.harness.Process(f.ctx, p))
}

// live returns the fixture owner's non-voided transactions.
func (f *botFixture) live(t *testing.T) []entity.Transaction {
	t.Helper()
	return f.liveFor(t, f.userID)
}

// liveFor is live for any account: the ledger is per user, so a reader
// has to name whose ledger it means.
func (f *botFixture) liveFor(t *testing.T, userID uuid.UUID) []entity.Transaction {
	t.Helper()
	ctx := scope.WithUserID(context.Background(), userID)
	rows, err := f.pool.Query(ctx,
		`SELECT id FROM transactions WHERE user_id = $1 AND voided_at IS NULL ORDER BY created_at`,
		userID)
	require.NoError(t, err)
	defer rows.Close()
	ids := []uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	out := make([]entity.Transaction, 0, len(ids))
	for _, id := range ids {
		tx, err := f.txRepo.Get(ctx, id)
		require.NoError(t, err)
		out = append(out, *tx)
	}
	return out
}

// inboundRow reads an inbound row straight from the table. A dropped
// message has no tenant, so the scoped repository cannot see it — and
// that NULL is exactly what these tests assert.
func (f *botFixture) inboundRow(t *testing.T, messageID string) (string, *uuid.UUID) {
	t.Helper()
	var status string
	var userID *uuid.UUID
	require.NoError(t, f.pool.QueryRow(context.Background(),
		`SELECT status, user_id FROM inbound_messages WHERE message_id = $1`, messageID,
	).Scan(&status, &userID))
	return status, userID
}

func TestCaptureWritesExpenseAndReplaysDuplicate(t *testing.T) {
	f := newBotFixture(t, nil)

	f.say(t, "m1", "kopi 25rb pakai cash")

	txs := f.live(t)
	require.Len(t, txs, 1)
	assert.Equal(t, "expense", txs[0].Type)
	assert.Equal(t, int64(25_000), txs[0].AmountMinor)
	assert.Equal(t, "whatsapp_text", txs[0].Origin)
	require.NotNil(t, txs[0].ExternalID)
	assert.Equal(t, "m1#1", *txs[0].ExternalID)
	require.NotNil(t, txs[0].Merchant)
	// The stored merchant is the canonical entity's name, tidied offline
	// because no model is configured in this fixture.
	assert.Equal(t, "Kopi", *txs[0].Merchant)
	// No LLM configured: the label is unknown, so a human must look.
	assert.Nil(t, txs[0].CategoryID)
	assert.Equal(t, "needs_review", txs[0].ReviewStatus)
	// content_hash is write-only on the entity; read it from the row.
	var hash *string
	require.NoError(t, f.pool.QueryRow(f.ctx,
		`SELECT content_hash FROM transactions WHERE id = $1`, txs[0].ID).Scan(&hash))
	assert.NotNil(t, hash)

	reply := f.sender.last()
	assert.True(t, strings.HasPrefix(reply, "Sip, tercatat: Rp25.000"), "reply was %q", reply)
	assert.Contains(t, reply, "Cash")

	// A retried delivery of the same message_id must not write again; it
	// replays the stored reply under an idempotency key.
	f.say(t, "m1", "kopi 25rb pakai cash")
	assert.Len(t, f.live(t), 1)
	sent := f.sender.all()
	require.Len(t, sent, 2)
	assert.Equal(t, sent[0].Text, sent[1].Text)
	assert.Equal(t, "reply:m1", sent[1].IdempotencyKey)
}

func TestMultilineBatchCaptureUsesTrailingSourceWithoutIntent(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		merchants []string
		amounts   []int64
	}{
		{
			name:      "bare source footer",
			input:     "sarapan bubur ayam 16k\ncilok 10k\nBRI",
			merchants: []string{"Sarapan bubur ayam", "Cilok"},
			amounts:   []int64{16_000, 10_000},
		},
		{
			name:      "dari source footer",
			input:     "nasi daun jeruk 22k\nes podeng 12k\nDari bri",
			merchants: []string{"Nasi daun jeruk", "Es podeng"},
			amounts:   []int64{22_000, 12_000},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeVisionLLM{
				receipt:  func() string { return "" },
				sourceID: func() uuid.UUID { return uuid.Nil },
				intent:   func() string { return unknownIntent },
			}
			llmServer := httptest.NewServer(http.HandlerFunc(fake.serve))
			t.Cleanup(llmServer.Close)
			f := newBotFixture(t, llm.New(llmServer.URL, "test-key", "test-model", 2*time.Second))

			f.say(t, "m1", tc.input)

			txs := f.live(t)
			require.Len(t, txs, 2)
			for i, tx := range txs {
				assert.Equal(t, tc.amounts[i], tx.AmountMinor)
				require.NotNil(t, tx.Merchant)
				assert.Equal(t, tc.merchants[i], *tx.Merchant)
				require.NotNil(t, tx.ExternalID)
				assert.Equal(t, fmt.Sprintf("m1#%d", i+1), *tx.ExternalID)
				assert.Equal(t, f.bri.ID, legSourceID(t, tx.Legs))
			}
			assert.Contains(t, f.sender.last(), "2 transaksi tercatat")
			open, err := f.pending.ListOpen(f.ctx)
			require.NoError(t, err)
			assert.Empty(t, open)
			assert.Zero(t, fake.intents(), "a unique trailing BRI Source is deterministic")
			assert.Zero(t, fake.sourcePicks(), "a trailing BRI footer is never repaired by source AI")
		})
	}
}

func TestFailedTextReplyStaysReadyAndRetriesStoredReply(t *testing.T) {
	f := newBotFixture(t, nil)
	f.sender.setSendError(errors.New("gateway unavailable"))

	f.say(t, "m1", "kopi 25k pakai cash, gojek 18k pakai gopay")

	row, err := f.inbound.Get(f.ctx, "m1")
	require.NoError(t, err)
	require.NotNil(t, row)
	require.Equal(t, "ready", row.Status)
	require.NotNil(t, row.ReplyText)
	assert.Len(t, row.TransactionIDs, 2)
	assert.Empty(t, f.sender.all(), "a failed delivery was not accepted by the gateway")
	assert.Len(t, f.live(t), 2)

	f.sender.setSendError(nil)
	f.say(t, "m1", "kopi 25k pakai cash, gojek 18k pakai gopay")
	assert.Empty(t, f.sender.all(), "a not-yet-due ready delivery cannot be retried immediately")
	require.NoError(t, f.pool.QueryRow(f.ctx,
		`UPDATE inbound_messages SET next_attempt_at = now() - interval '1 second' WHERE message_id = $1 RETURNING message_id`,
		"m1",
	).Scan(new(string)))
	f.say(t, "m1", "kopi 25k pakai cash, gojek 18k pakai gopay")

	assert.Len(t, f.live(t), 2, "delivery recovery never reruns the ledger")
	sent := f.sender.all()
	require.Len(t, sent, 1)
	assert.Equal(t, *row.ReplyText, sent[0].Text)
	assert.Equal(t, "reply:m1", sent[0].IdempotencyKey)
}

func TestFailedInformationalReplyClearsReactionOnRecovery(t *testing.T) {
	f := newBotFixture(t, nil)
	f.sender.setSendError(errors.New("gateway unavailable"))

	f.say(t, "m1", "rekap")
	row, err := f.inbound.Get(f.ctx, "m1")
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, "ready", row.Status)
	assert.Equal(t, []string{"👀"}, f.sender.reactionsOn("m1"))

	f.sender.setSendError(nil)
	require.NoError(t, f.pool.QueryRow(f.ctx,
		`UPDATE inbound_messages SET next_attempt_at = now() - interval '1 second' WHERE message_id = $1 RETURNING message_id`,
		"m1",
	).Scan(new(string)))
	f.say(t, "m1", "rekap")

	assert.Equal(t, []string{"👀", ""}, f.sender.reactionsOn("m1"))
	row, err = f.inbound.Get(f.ctx, "m1")
	require.NoError(t, err)
	assert.Equal(t, "replied", row.Status)
}


func TestUnknownSenderDropsDueExistingLeaseWithoutOutbound(t *testing.T) {
	f := newBotFixture(t, nil)
	const messageID = "m-unknown-recovery"
	owner := ownerPhone[1:] + "@s.whatsapp.net"
	claim, err := f.inbound.ClaimInbound(f.ctx, &repository.InboundMessage{
		MessageID: messageID, Chat: owner, FromJID: owner, UserID: &f.userID,
		OccurredAt: time.Now().UTC(), Body: "kopi 25k", Kind: "text",
	})
	require.NoError(t, err)
	require.Equal(t, repository.InboundClaimWork, claim.Kind)
	_, err = f.pool.Exec(f.ctx, `UPDATE inbound_messages
		SET next_attempt_at = now() - interval '1 second', lease_until = now() - interval '1 second'
		WHERE message_id = $1`, messageID)
	require.NoError(t, err)

	require.NoError(t, f.harness.Process(f.ctx, &waga.IncomingWebhookPayload{
		Event: waga.IncomingEventMessageMessageIncoming, Chat: owner,
		From: "628999999999:1@s.whatsapp.net", MessageId: messageID,
		Timestamp: time.Now().Unix(), Text: "kopi 25k", Type: waga.IncomingMessageTypeText,
	}))

	row, err := f.inbound.Get(f.ctx, messageID)
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, "dropped", row.Status)
	assert.Nil(t, row.LeaseUntil)
	assert.Empty(t, f.sender.all())
}
func TestBatchCaptureThenBatalOneItem(t *testing.T) {
	f := newBotFixture(t, nil)

	f.say(t, "m1", "kopi 25k pakai cash, gojek 18k pakai gopay")

	txs := f.live(t)
	require.Len(t, txs, 2)
	assert.Equal(t, "m1#1", *txs[0].ExternalID)
	assert.Equal(t, "m1#2", *txs[1].ExternalID)
	assert.Contains(t, f.sender.last(), "2 transaksi tercatat")

	f.say(t, "m2", "batal 2")

	reply := f.sender.last()
	assert.Contains(t, reply, "kubatalkan")
	// The reversal is a new, linked row: nothing is deleted.
	var reversals int
	require.NoError(t, f.pool.QueryRow(f.ctx,
		`SELECT COUNT(*) FROM transactions WHERE reverses_transaction_id = $1`, txs[1].ID,
	).Scan(&reversals))
	assert.Equal(t, 1, reversals)
	require.NoError(t, f.pool.QueryRow(f.ctx,
		`SELECT COUNT(*) FROM transactions WHERE reverses_transaction_id = $1`, txs[0].ID,
	).Scan(&reversals))
	assert.Equal(t, 0, reversals, "only the numbered item is reversed")
}

func TestTransferWithoutDestinationAsksThenWrites(t *testing.T) {
	f := newBotFixture(t, nil)

	f.say(t, "m1", "pindah 500k dari bca")

	reply := f.sender.last()
	assert.Contains(t, reply, "Aku tahan dulu")
	assert.Contains(t, reply, "Dipindah ke mana?")
	assert.Empty(t, f.live(t), "a held draft writes nothing")

	open, err := f.pending.ListOpen(f.ctx)
	require.NoError(t, err)
	require.Len(t, open, 1)
	assert.Equal(t, "capture", open[0].Kind)
	var params map[string]any
	require.NoError(t, json.Unmarshal(open[0].Params, &params))
	assert.Equal(t, []any{"destination"}, params["missing"])
	assert.Equal(t, "pindah 500k dari bca", params["original_text"])

	state, err := f.chats.Get(f.ctx, ownerPhone[1:]+"@s.whatsapp.net")
	require.NoError(t, err)
	require.NotNil(t, state.OpenExpect)
	assert.Equal(t, "destination", *state.OpenExpect)

	f.say(t, "m2", "gopay")

	txs := f.live(t)
	require.Len(t, txs, 1)
	assert.Equal(t, "transfer", txs[0].Type)
	assert.Equal(t, int64(500_000), txs[0].AmountMinor)
	assert.Contains(t, f.sender.last(), "BCA")
	assert.Contains(t, f.sender.last(), "GoPay")

	open, err = f.pending.ListOpen(f.ctx)
	require.NoError(t, err)
	assert.Empty(t, open, "the parked draft is resolved once executed")

	var action string
	require.NoError(t, f.pool.QueryRow(f.ctx,
		`SELECT action FROM pending_actions LIMIT 1`).Scan(&action))
	assert.Equal(t, "confirm", action)
}

func TestAmbiguousAmountsHoldWithoutWriting(t *testing.T) {
	f := newBotFixture(t, nil)

	f.say(t, "m1", "kopi 25k bayar 50k pakai cash")

	assert.Empty(t, f.live(t))
	reply := f.sender.last()
	assert.Contains(t, reply, "Aku tahan dulu")
	assert.Contains(t, reply, "mana yang dibayar")

	open, err := f.pending.ListOpen(f.ctx)
	require.NoError(t, err)
	require.Len(t, open, 1)
	var params map[string]any
	require.NoError(t, json.Unmarshal(open[0].Params, &params))
	assert.Equal(t, []any{"amount"}, params["missing"])
}

func TestRefundConfirmsIntoLinkedReversal(t *testing.T) {
	f := newBotFixture(t, nil)

	f.say(t, "m1", "gojek 18k pakai gopay")
	original := f.live(t)[0]

	f.say(t, "m2", "refund gojek 18k")
	reply := f.sender.last()
	assert.Contains(t, reply, "Refund")
	assert.Contains(t, reply, "Balas ya/tidak")

	open, err := f.pending.ListOpen(f.ctx)
	require.NoError(t, err)
	require.Len(t, open, 1)
	assert.Equal(t, "refund", open[0].Kind)

	f.say(t, "m3", "ya")

	var reverses uuid.UUID
	require.NoError(t, f.pool.QueryRow(f.ctx,
		`SELECT reverses_transaction_id FROM transactions WHERE reverses_transaction_id IS NOT NULL`,
	).Scan(&reverses))
	assert.Equal(t, original.ID, reverses)
	assert.Contains(t, f.sender.last(), "sudah kukembalikan")
}

func TestRefundWithoutTargetSaysSo(t *testing.T) {
	f := newBotFixture(t, nil)

	f.say(t, "m1", "refund gojek 18k")

	assert.Empty(t, f.live(t))
	assert.Contains(t, f.sender.last(), "nggak nemu transaksi gojek")
	open, err := f.pending.ListOpen(f.ctx)
	require.NoError(t, err)
	assert.Empty(t, open)
}

// A group is never a ledger: even the owner's own message is dropped
// there, because the bot would be answering in front of strangers.
func TestGroupMessagesAreDropped(t *testing.T) {
	f := newBotFixture(t, nil)

	f.deliver(t, &waga.IncomingWebhookPayload{
		Event: waga.IncomingEventMessageMessageIncoming,
		Chat:  "1234-5678@g.us", From: ownerPhone[1:] + ":1@s.whatsapp.net", IsGroup: true,
		MessageId: "x2", Timestamp: time.Now().Unix(),
		Text: "kopi 25rb", Type: waga.IncomingMessageTypeText,
	})

	assert.Empty(t, f.live(t))
	assert.Empty(t, f.sender.all(), "a dropped message is never answered")
	status, userID := f.inboundRow(t, "x2")
	assert.Equal(t, "dropped", status)
	assert.Nil(t, userID, "a group message belongs to no ledger")
}

// A stranger must not be able to tell that this number is a bot: no
// reply, no reaction, and nothing of theirs in anyone's ledger.
func TestUnknownSenderIsDroppedSilently(t *testing.T) {
	f := newBotFixture(t, nil)

	f.sayAs(t, "+628999999999", "x1", "kopi 25rb pakai cash")

	assert.Empty(t, f.sender.all(), "an unknown sender is never answered")
	assert.Empty(t, f.live(t), "a stranger cannot write into the owner's ledger")
	status, userID := f.inboundRow(t, "x1")
	assert.Equal(t, "dropped", status)
	assert.Nil(t, userID)
}

// The one number a stranger gate lets through: someone who was invited
// and has not signed up yet is waiting for exactly this link.
func TestInvitedSenderReceivesTheLink(t *testing.T) {
	f := newBotFixture(t, nil)

	const invited = "+628999999999"
	first, _, err := f.inviteSvc.Issue(f.ctx, invited, f.userID)
	require.NoError(t, err)
	beforeInbound := len(f.sender.all())
	require.Equal(t, 1, beforeInbound, "issuing an invite delivers the first link")

	f.sayAs(t, invited, "x1", "halo?")

	sent := f.sender.all()
	require.Len(t, sent, beforeInbound+1, "the invitee gets exactly one fresh link")
	assert.Contains(t, sent[len(sent)-1].Text, "/register?invite=")
	assert.Empty(t, f.live(t), "an invitee has no ledger yet and writes into nobody else's")
	status, userID := f.inboundRow(t, "x1")
	assert.Equal(t, "dropped", status, "the link is not a reply: the message itself is dropped")
	assert.Nil(t, userID)
	// A stored token hash cannot be turned back into a link, so the
	// resend is a re-issue: the number keeps exactly one open invite and
	// it is a new one.
	open, err := f.invites.OpenByPhoneDigits(context.Background(), invited[1:])
	require.NoError(t, err)
	require.NotNil(t, open, "the number lost its invite")
	assert.NotEqual(t, first.ID, open.ID, "the old invite row should have been replaced")
}

// Video, audio, documents and stickers are still refused; photos are
// not (see photo_test.go).
func TestNonPhotoMediaIsRefusedPolitely(t *testing.T) {
	f := newBotFixture(t, nil)

	f.deliver(t, &waga.IncomingWebhookPayload{
		Event: waga.IncomingEventMessageMessageIncoming,
		Chat:  ownerPhone[1:] + "@s.whatsapp.net", From: ownerPhone[1:] + ":1@s.whatsapp.net",
		MessageId: "m1", Timestamp: time.Now().Unix(),
		Type: waga.IncomingMessageTypeVideo,
	})

	assert.Empty(t, f.live(t))
	assert.Contains(t, f.sender.last(), "Kirim sebagai foto ya")
}
func TestImageWithoutMediaGetsUnsupportedReplyAndRejectedReaction(t *testing.T) {
	f := newBotFixture(t, nil)

	f.deliver(t, &waga.IncomingWebhookPayload{
		Event: waga.IncomingEventMessageMessageIncoming,
		Chat:  ownerPhone[1:] + "@s.whatsapp.net", From: ownerPhone[1:] + ":1@s.whatsapp.net",
		MessageId: "m1", Timestamp: time.Now().Unix(),
		Type:      waga.IncomingMessageTypeImage,
	})

	assert.Contains(t, f.sender.last(), "Kirim sebagai foto ya")
	assert.Equal(t, []string{"👀", "❌"}, f.sender.reactionsOn("m1"))
	row, err := f.inbound.Get(f.ctx, "m1")
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, "replied", row.Status)
	require.NotNil(t, row.OutcomeReaction)
	assert.Equal(t, "❌", *row.OutcomeReaction)
}


// WhatsApp may address an account by an opaque lid instead of its phone
// number; the stored lid is what makes that the same person.
func TestLidSenderIsMatched(t *testing.T) {
	f := newBotFixture(t, nil)
	require.NoError(t, f.users.SetLID(f.ctx, f.userID, "136781890584786"))

	f.deliver(t, &waga.IncomingWebhookPayload{
		Event: waga.IncomingEventMessageMessageIncoming,
		Chat:  "136781890584786@lid", From: "136781890584786:12@lid",
		AddressingMode: "lid",
		MessageId:      "lid-1", Timestamp: time.Now().Unix(),
		Text: "kopi 25rb pakai cash", Type: waga.IncomingMessageTypeText,
	})

	require.Len(t, f.live(t), 1, "the capture lands in the matched account's ledger")
	assert.Contains(t, f.sender.last(), "Sip, tercatat: Rp25.000")
}

// The lid is learned from the gateway on the first phone-addressed
// message, so a later lid-addressed one still resolves without the
// operator touching anything.
func TestLidIsLearnedFromPhoneMessage(t *testing.T) {
	f := newBotFixture(t, nil)
	f.sender.lid = "999"

	f.say(t, "m1", "kopi 25rb pakai cash")

	user, err := f.users.GetByID(f.ctx, f.userID)
	require.NoError(t, err)
	require.NotNil(t, user)
	require.NotNil(t, user.WhatsAppLID, "the lid was never stored")
	assert.Equal(t, "999", *user.WhatsAppLID)
}

func TestLlmFailureStillWritesUncategorised(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	f := newBotFixture(t, llm.New(srv.URL, "test-key", "test-model", 2*time.Second))

	f.say(t, "m1", "kopi 25rb pakai cash")

	txs := f.live(t)
	require.Len(t, txs, 1)
	assert.Nil(t, txs[0].CategoryID)
	assert.Nil(t, txs[0].CategorySource)
	assert.Equal(t, "needs_review", txs[0].ReviewStatus)
	assert.GreaterOrEqual(t, calls, 2, "the client retries once before giving up")
	assert.Contains(t, f.sender.last(), "belum ada kategori")
}

func TestLlmCategoryIsWhitelistedAndRecorded(t *testing.T) {
	// The handler reads `chosen` on call, so the fixture's real category
	// id can be assigned after the server starts.
	var chosen uuid.UUID
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		body := fmt.Sprintf(`{"category_id":%q,"confidence":0.91}`, chosen)
		payload := map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": body}}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	}))
	defer srv.Close()

	f := newBotFixture(t, llm.New(srv.URL, "test-key", "test-model", 2*time.Second))
	chosen = f.makanan.ID

	f.say(t, "m1", "kopi 25rb pakai cash")

	txs := f.live(t)
	require.Len(t, txs, 1)
	require.NotNil(t, txs[0].CategoryID)
	assert.Equal(t, f.makanan.ID, *txs[0].CategoryID)
	require.NotNil(t, txs[0].CategorySource)
	assert.Equal(t, "llm", *txs[0].CategorySource)
	assert.Equal(t, "clean", txs[0].ReviewStatus)
	assert.Contains(t, f.sender.last(), "Makanan & Minuman")
}

func TestMerchantMemoryBeatsTheModel(t *testing.T) {
	f := newBotFixture(t, nil)

	// A user-set label on this merchant is a rule, not a guess.
	merchant := "kopi"
	_, err := service.NewTransactionService(
		repository.NewTransactionRepository(f.pool),
		repository.NewSourceRepository(f.pool),
		repository.NewCategoryRepository(f.pool),
		service.NewMerchantService(repository.NewMerchantRepository(f.pool), nil, f.logger),
		repository.NewBillPaymentRepository(f.pool),
		repository.NewBillRepository(f.pool),
		f.logger,
	).Create(f.ctx, service.CreateInput{
		Type: "expense", AmountMinor: 20_000, SourceID: f.cash.ID,
		CategoryID: &f.makanan.ID, Merchant: &merchant, Origin: "dashboard_manual",
	})
	require.NoError(t, err)

	f.say(t, "m1", "kopi 25rb pakai cash")

	txs := f.live(t)
	require.Len(t, txs, 2)
	captured := txs[1]
	require.NotNil(t, captured.CategoryID)
	assert.Equal(t, f.makanan.ID, *captured.CategoryID)
	require.NotNil(t, captured.CategorySource)
	assert.Equal(t, "rules", *captured.CategorySource)
	assert.Equal(t, "clean", captured.ReviewStatus)
}

// legSourceID returns the party a transaction is spent from.
func legSourceID(t *testing.T, legs []entity.Leg) uuid.UUID {
	t.Helper()
	for _, leg := range legs {
		if leg.SourceID != nil && leg.SignedAmount < 0 {
			return *leg.SourceID
		}
	}
	for _, leg := range legs {
		if leg.SourceID != nil {
			return *leg.SourceID
		}
	}
	t.Fatalf("no source leg")
	return uuid.Nil
}

// The Source memory is not a photo feature: a typed capture for a shop
// the user has paid before needs no question either.
func TestTypedCaptureUsesMerchantMemory(t *testing.T) {
	f := newBotFixture(t, nil)

	f.say(t, "m1", "alfamart 20k pakai bca")
	require.Len(t, f.live(t), 1)

	f.say(t, "m2", "alfamart 15k")

	txs := f.live(t)
	require.Len(t, txs, 2, "the second capture writes without asking")
	assert.Equal(t, int64(15_000), txs[1].AmountMinor)
	assert.Equal(t, f.bca.ID, legSourceID(t, txs[1].Legs))
	reply := f.sender.last()
	assert.Contains(t, reply, "Sip, tercatat: Rp15.000")
	assert.Contains(t, reply, "BCA")

	open, err := f.pending.ListOpen(f.ctx)
	require.NoError(t, err)
	assert.Empty(t, open, "nothing is parked when the memory answers")
}

// Messages in one chat are processed in the order they arrived, even
// when the first one is slow: the answer must not overtake the question
// it belongs to.
func TestChatProcessingIsSerialised(t *testing.T) {
	f := newBotFixture(t, nil)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		f.say(t, "m1", "pindah 500k dari bca")
	}()
	go func() {
		defer wg.Done()
		time.Sleep(20 * time.Millisecond)
		f.say(t, "m2", "gopay")
	}()
	wg.Wait()

	txs := f.live(t)
	require.Len(t, txs, 1, "the answer found the question waiting for it")
	assert.Equal(t, "transfer", txs[0].Type)
	assert.Equal(t, int64(500_000), txs[0].AmountMinor)
}

// Every accepted text message is stamped the moment it arrives and again
// once its outcome is known — a write, a parked question, or nothing at
// all for a message that only informed.
func TestTextMessagesAreAcknowledgedAndStamped(t *testing.T) {
	f := newBotFixture(t, nil)

	f.say(t, "m1", "kopi 25k pakai cash")
	assert.Equal(t, []string{"👀", "✅"}, f.sender.reactionsOn("m1"))
	assert.Equal(t, []bool{true, false}, f.sender.typing)

	f.say(t, "m2", "rekap")
	assert.Equal(t, []string{"👀", ""}, f.sender.reactionsOn("m2"), "a recap changes nothing")

	f.say(t, "m3", "batal")
	assert.Equal(t, []string{"👀", "✅"}, f.sender.reactionsOn("m3"), "a reversal is a ledger change")
}

// A question parks the message that asked it; answering it stamps both:
// the answer wrote, and the original is no longer outstanding.
func TestParkedQuestionStampsBothMessages(t *testing.T) {
	f := newBotFixture(t, nil)

	f.say(t, "m1", "pindah 500k dari bca")
	assert.Equal(t, []string{"👀", "📋"}, f.sender.reactionsOn("m1"))

	f.say(t, "m2", "gopay")
	assert.Equal(t, []string{"👀", "📋", "✅"}, f.sender.reactionsOn("m1"))
	assert.Equal(t, []string{"👀", "✅"}, f.sender.reactionsOn("m2"))
}

// Refusing a held capture stamps the message that opened it, the same
// way a refused photo is stamped.
func TestHeldCaptureDiscardStampsTheOriginal(t *testing.T) {
	f := newBotFixture(t, nil)

	f.say(t, "m1", "tissu 20k")
	assert.Contains(t, f.sender.last(), "Bayar pakai sumber mana?")

	f.say(t, "m2", "tidak")

	got := f.sender.reactionsOn("m1")
	require.NotEmpty(t, got)
	assert.Equal(t, "❌", got[len(got)-1])
	assert.Empty(t, f.live(t))
}

// The isolation guarantee, end to end through the bot: two numbers, two
// ledgers, and neither the rows nor the summaries cross over.
func TestTwoUsersLedgersAreIsolated(t *testing.T) {
	f := newBotFixture(t, nil)

	const secondPhone = "+628123000000"
	secondCtx, secondID := testkit.Scoped(t, f.pool, secondPhone, "Second")
	_, err := f.sources.Create(secondCtx, &entity.Source{
		ID: uuid.New(), Name: "Cash", Type: "cash", Currency: "IDR",
	})
	require.NoError(t, err)

	f.sayAs(t, ownerPhone, "m1", "kopi 25rb pakai cash")
	f.sayAs(t, secondPhone, "m2", "bakso 30rb pakai cash")

	owner := f.liveFor(t, f.userID)
	require.Len(t, owner, 1, "the owner sees only their own capture")
	require.NotNil(t, owner[0].Merchant)
	assert.Equal(t, "Kopi", *owner[0].Merchant)

	second := f.liveFor(t, secondID)
	require.Len(t, second, 1, "the second account sees only its own capture")
	require.NotNil(t, second[0].Merchant)
	assert.Equal(t, "Bakso", *second[0].Merchant)

	// A summary is the easiest place for a leak to hide: it aggregates
	// across the whole week rather than naming a row.
	f.sayAs(t, secondPhone, "m3", "rekap minggu")
	recap := f.sender.last()
	assert.Contains(t, recap, "Bakso", "the second account's own merchant is missing: %q", recap)
	assert.NotContains(t, recap, "kopi", "another account's merchant leaked into the recap")
}
