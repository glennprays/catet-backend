package repository_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/glennprays/catet-backend/internal/scope"
	"github.com/glennprays/catet-backend/internal/testkit"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tenants are kept apart by application code alone — there is no RLS, so
// the only thing between two ledgers is that every repository reads its
// tenant from the context and puts it in the WHERE clause. One forgotten
// predicate leaks somebody's money history, and a leak is silent: the
// query still succeeds. This file is the gate: one row of everything
// under tenant B, then every reader run again as tenant A.

// Horizons are the cheapest proof that user_settings is per-user: B
// moves its horizon, A must still read the migration default.
const (
	defaultHorizonDays = 7
	tenantBHorizonDays = 30
)

// ledgerRepos is every repository that touches a user-owned table.
type ledgerRepos struct {
	sources   *repository.SourceRepository
	cats      *repository.CategoryRepository
	txs       *repository.TransactionRepository
	analytics *repository.AnalyticsRepository
	budgets   *repository.BudgetRepository
	bills     *repository.BillRepository
	billPays  *repository.BillPaymentRepository
	reminders *repository.BillReminderSendRepository
	pending   *repository.PendingActionRepository
	chats     *repository.BotChatStateRepository
	inbound   *repository.InboundMessageRepository
	settings  *repository.SettingsRepository
	pool      *pgxpool.Pool
}

func newLedgerRepos(pool *pgxpool.Pool) ledgerRepos {
	return ledgerRepos{
		sources:   repository.NewSourceRepository(pool),
		cats:      repository.NewCategoryRepository(pool),
		txs:       repository.NewTransactionRepository(pool),
		analytics: repository.NewAnalyticsRepository(pool),
		budgets:   repository.NewBudgetRepository(pool),
		bills:     repository.NewBillRepository(pool),
		billPays:  repository.NewBillPaymentRepository(pool),
		reminders: repository.NewBillReminderSendRepository(pool),
		pending:   repository.NewPendingActionRepository(pool),
		chats:     repository.NewBotChatStateRepository(pool),
		inbound:   repository.NewInboundMessageRepository(pool),
		settings:  repository.NewSettingsRepository(pool),
		pool:      pool,
	}
}

// tenantRows holds the ids and lookup keys of the rows tenant B owns —
// the handles tenant A is then handed and must not be able to resolve.
// A by-id reader only proves isolation when the id it is given really
// exists, so the fixture hands out the real ones.
type tenantRows struct {
	source      uuid.UUID
	category    uuid.UUID
	tx          uuid.UUID
	bill        uuid.UUID
	budget      uuid.UUID
	pending     uuid.UUID
	chat        string
	messageID   string
	externalID  string
	contentHash string
	merchant    string
	mediaSha    string
	occurredAt  time.Time
	nextDueDate time.Time
	period      string
}

// seedTenant writes one row in every user-owned table under the scope it
// is given.
func seedTenant(t *testing.T, ctx context.Context, r ledgerRepos, userID uuid.UUID) tenantRows {
	t.Helper()

	const amount = 25_000
	now := time.Now().UTC().Truncate(time.Second)
	rows := tenantRows{
		chat:        "628111000902@s.whatsapp.net",
		messageID:   "wamid.tenant-b-1",
		externalID:  "wa:tenant-b-1",
		contentHash: "sha256:tenant-b-receipt",
		merchant:    "warung tenant b",
		mediaSha:    "9f86d081884c7d659a2feaa0c55ad015",
		occurredAt:  now,
		nextDueDate: now.AddDate(0, 0, 3),
		period:      now.Format("2006-01"),
	}

	src, err := r.sources.Create(ctx, &entity.Source{
		ID: uuid.New(), Name: "BCA", Type: "bank", Currency: "IDR",
	})
	require.NoError(t, err)
	rows.source = src.ID

	cat, err := r.cats.Create(ctx, &entity.Category{ID: uuid.New(), Name: "Makanan"})
	require.NoError(t, err)
	rows.category = cat.ID

	// A complete expense: source leg out, category leg in. category_source
	// is 'user' because the merchant-memory reader only trusts labels the
	// user set, and review_status is needs_review so the review queue
	// counter has something to count.
	tx, err := r.txs.Insert(ctx, &entity.Transaction{
		ID:             uuid.New(),
		OccurredAt:     rows.occurredAt,
		Origin:         "whatsapp_text",
		Type:           "expense",
		Merchant:       &rows.merchant,
		CategoryID:     &rows.category,
		CategorySource: new("user"),
		ReviewStatus:   "needs_review",
		ExternalID:     &rows.externalID,
		ContentHash:    &rows.contentHash,
		Legs: []entity.Leg{
			{ID: uuid.New(), SourceID: &rows.source, SignedAmount: -amount},
			{ID: uuid.New(), CategoryID: &rows.category, SignedAmount: amount},
		},
	})
	require.NoError(t, err)
	rows.tx = tx.ID

	// A second capture with no category at all. UncategorizedCount counts
	// only those, so without this row it would answer zero for both
	// tenants and a missing user_id filter would slip through unnoticed.
	_, err = r.txs.Insert(ctx, &entity.Transaction{
		ID:           uuid.New(),
		OccurredAt:   rows.occurredAt,
		Origin:       "whatsapp_text",
		Type:         "expense",
		ReviewStatus: "needs_review",
		Legs: []entity.Leg{
			{ID: uuid.New(), SourceID: &rows.source, SignedAmount: -5_000},
			{ID: uuid.New(), SignedAmount: 5_000},
		},
	})
	require.NoError(t, err)

	bill, err := r.bills.Create(ctx, &entity.Bill{
		ID: uuid.New(), Name: "Internet",
		AmountMinMinor: 300_000, AmountMaxMinor: 350_000,
		Cadence: "monthly", NextDueDate: rows.nextDueDate,
		LinkedSourceID: &rows.source, CategoryID: &rows.category,
	})
	require.NoError(t, err)
	rows.bill = bill.ID

	// A settled occurrence and a delivered reminder slot: both are
	// user-owned tables, so both need a row tenant A must not see.
	require.NoError(t, r.billPays.Create(ctx, &entity.BillPayment{
		ID: uuid.New(), BillID: bill.ID, TransactionID: rows.tx,
		DueDate: rows.nextDueDate, AmountMinor: 320_000,
	}))
	dbTx, err := r.pool.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, r.reminders.InsertAll(ctx, dbTx, []entity.BillReminderSend{{
		BillID: bill.ID, DueDate: rows.nextDueDate, OffsetDays: 1, SendDate: rows.nextDueDate.AddDate(0, 0, -1),
	}}))
	require.NoError(t, dbTx.Commit(ctx))

	budget, err := r.budgets.Create(ctx,
		&entity.BudgetRow{CategoryID: rows.category, AmountMinor: 1_000_000},
		rows.period, false,
	)
	require.NoError(t, err)
	rows.budget = budget.ID

	action, err := r.pending.Create(ctx, "create_transaction",
		json.RawMessage(`{"amount_minor":25000}`), "warung tenant b 25rb")
	require.NoError(t, err)
	rows.pending = action.ID

	require.NoError(t, r.chats.Upsert(ctx, &repository.ChatState{
		Chat: rows.chat, OpenExpect: new("source"),
	}))

	// status 'replied' plus a media hash: that is the shape the photo
	// replay reader looks for.
	inserted, _, err := r.inbound.InsertProcessing(ctx, &repository.InboundMessage{
		MessageID: rows.messageID, Chat: rows.chat, FromJID: rows.chat,
		UserID: &userID, OccurredAt: rows.occurredAt,
		Body: "kopi 25rb", Kind: "photo", Status: "replied",
		MediaSha256: &rows.mediaSha,
	})
	require.NoError(t, err)
	require.True(t, inserted, "fixture message must be new")

	require.NoError(t, r.settings.UpdateIncomeHorizon(ctx, tenantBHorizonDays))

	return rows
}

// readerCase is one repository read, run from both sides. The owner side
// is not ceremony: a reader that returns nothing to anybody — a broken
// join, a filter on the wrong column — would otherwise "prove" isolation
// while being useless, so each case first shows the row is reachable by
// its owner and only then that it is invisible to everybody else.
type readerCase struct {
	name     string
	owner    func(*testing.T, context.Context)
	stranger func(*testing.T, context.Context)
}

// isolated builds a case from any single-value reader: slices, maps,
// pointers and counters all answer to Empty/NotEmpty, so one pair of
// assertions covers every reader shape in the package.
func isolated[T any](name string, read func(context.Context) (T, error)) readerCase {
	return readerCase{
		name: name,
		owner: func(t *testing.T, ctx context.Context) {
			got, err := read(ctx)
			require.NoError(t, err)
			assert.NotEmpty(t, got, "B must be able to read its own row here, or this case proves nothing about isolation")
		},
		stranger: func(t *testing.T, ctx context.Context) {
			got, err := read(ctx)
			require.NoError(t, err)
			assert.Empty(t, got, "A must not see B's ledger")
		},
	}
}

// TestTenantIsolation is the load-bearing test of the multi-tenant
// design: every reader that touches a user-owned table, exercised by the
// row's owner and by a second tenant holding the same ids.
func TestTenantIsolation(t *testing.T) {
	pool := testkit.NewDB(t)
	r := newLedgerRepos(pool)

	ctxA, _ := testkit.Scoped(t, pool, "+628111000901", "Tenant A")
	ctxB, idB := testkit.Scoped(t, pool, "+628111000902", "Tenant B")
	rows := seedTenant(t, ctxB, r, idB)

	// The analytics window brackets the fixture's single instant; bills
	// are dated in whole days, so they get a window in days.
	from := rows.occurredAt.Add(-time.Hour)
	to := rows.occurredAt.Add(time.Hour)
	reminderSendDate := rows.nextDueDate.AddDate(0, 0, -1)

	readers := []readerCase{
		isolated("sources.List", func(ctx context.Context) ([]entity.Source, error) {
			return r.sources.List(ctx, false)
		}),
		isolated("sources.Get", func(ctx context.Context) (*entity.Source, error) {
			return r.sources.Get(ctx, rows.source)
		}),
		isolated("sources.Balances", func(ctx context.Context) (map[uuid.UUID]int64, error) {
			return r.sources.Balances(ctx)
		}),

		isolated("categories.List", func(ctx context.Context) ([]entity.Category, error) {
			return r.cats.List(ctx, false)
		}),
		isolated("categories.Get", func(ctx context.Context) (*entity.Category, error) {
			return r.cats.Get(ctx, rows.category)
		}),

		isolated("transactions.List", func(ctx context.Context) ([]repository.LedgerRow, error) {
			return r.txs.List(ctx, repository.ListFilter{Limit: 50})
		}),
		isolated("transactions.Count", func(ctx context.Context) (int64, error) {
			return r.txs.Count(ctx, repository.ListFilter{})
		}),
		isolated("transactions.Get", func(ctx context.Context) (*entity.Transaction, error) {
			return r.txs.Get(ctx, rows.tx)
		}),
		isolated("transactions.GetByExternalID", func(ctx context.Context) (*entity.Transaction, error) {
			return r.txs.GetByExternalID(ctx, rows.externalID)
		}),
		isolated("transactions.FindByContentHash", func(ctx context.Context) (*uuid.UUID, error) {
			return r.txs.FindByContentHash(ctx, rows.contentHash)
		}),
		isolated("transactions.LatestExpenseByMerchant", func(ctx context.Context) (*entity.Transaction, error) {
			return r.txs.LatestExpenseByMerchant(ctx, rows.merchant, from)
		}),
		isolated("transactions.LastSourceForMerchant", func(ctx context.Context) (*uuid.UUID, error) {
			return r.txs.LastSourceForMerchant(ctx, rows.merchant)
		}),
		isolated("transactions.LastUserCategoryForMerchant", func(ctx context.Context) (*uuid.UUID, error) {
			return r.txs.LastUserCategoryForMerchant(ctx, rows.merchant)
		}),

		isolated("analytics.CategorySpend", func(ctx context.Context) ([]repository.CategorySpendRow, error) {
			return r.analytics.CategorySpend(ctx, from, to)
		}),
		isolated("analytics.DailyFlows", func(ctx context.Context) (map[string]int64, error) {
			_, expense, err := r.analytics.DailyFlows(ctx, from, to)
			return expense, err
		}),
		isolated("analytics.TopMerchants", func(ctx context.Context) ([]repository.TopMerchantRow, error) {
			return r.analytics.TopMerchants(ctx, from, to, 5)
		}),
		isolated("analytics.NeedsReviewCount", func(ctx context.Context) (int64, error) {
			return r.analytics.NeedsReviewCount(ctx)
		}),
		isolated("analytics.UncategorizedCount", func(ctx context.Context) (int64, error) {
			return r.analytics.UncategorizedCount(ctx, from)
		}),

		isolated("budgets.List", func(ctx context.Context) ([]repository.BudgetRecord, error) {
			return r.budgets.List(ctx, rows.period)
		}),
		isolated("budgets.GetByID", func(ctx context.Context) (*repository.BudgetRecord, error) {
			return r.budgets.GetByID(ctx, rows.budget)
		}),

		isolated("bills.List", func(ctx context.Context) ([]entity.Bill, error) {
			return r.bills.List(ctx)
		}),
		isolated("bills.Get", func(ctx context.Context) (*entity.Bill, error) {
			return r.bills.Get(ctx, rows.bill)
		}),
		isolated("bills.ListActive", func(ctx context.Context) ([]entity.Bill, error) {
			return r.bills.ListActive(ctx)
		}),
		isolated("billPayments.ByTransaction", func(ctx context.Context) (*entity.BillPayment, error) {
			return r.billPays.ByTransaction(ctx, rows.tx)
		}),
		isolated("billPayments.ListBetween", func(ctx context.Context) ([]entity.BillPayment, error) {
			// paid_at defaults to now(), so the payment sits in the
			// instant window, not the due-date one.
			return r.billPays.ListBetween(ctx, from, to)
		}),
		isolated("billReminderSends.RemindedBillIDs", func(ctx context.Context) ([]uuid.UUID, error) {
			return r.reminders.RemindedBillIDs(ctx, reminderSendDate)
		}),

		isolated("pendingActions.ListOpen", func(ctx context.Context) ([]entity.PendingAction, error) {
			return r.pending.ListOpen(ctx)
		}),
		isolated("pendingActions.CountOpen", func(ctx context.Context) (int64, error) {
			return r.pending.CountOpen(ctx)
		}),
		isolated("pendingActions.GetOpen", func(ctx context.Context) (*entity.PendingAction, error) {
			return r.pending.GetOpen(ctx, rows.pending)
		}),

		isolated("botChatState.Get", func(ctx context.Context) (*repository.ChatState, error) {
			return r.chats.Get(ctx, rows.chat)
		}),

		isolated("inboundMessages.Get", func(ctx context.Context) (*repository.InboundMessage, error) {
			return r.inbound.Get(ctx, rows.messageID)
		}),
		isolated("inboundMessages.FindRepliedByMediaSha256", func(ctx context.Context) (*repository.InboundMessage, error) {
			return r.inbound.FindRepliedByMediaSha256(ctx, rows.mediaSha, from)
		}),
	}

	for _, rc := range readers {
		t.Run(rc.name, func(t *testing.T) {
			rc.owner(t, ctxB)
			rc.stranger(t, ctxA)
		})
	}

	// Settings are the one reader whose isolated answer is not empty:
	// every account has a row, so a leak here shows up as A reading B's
	// number instead of as A reading nothing.
	t.Run("settings.Get", func(t *testing.T) {
		horizon, err := r.settings.Get(ctxB)
		require.NoError(t, err)
		assert.Equal(t, tenantBHorizonDays, horizon, "B must read back the horizon B set")

		horizon, err = r.settings.Get(ctxA)
		require.NoError(t, err)
		assert.Equal(t, defaultHorizonDays, horizon, "A keeps its own default — settings are not shared state")
	})
}

// TestUnscopedContextIsRefused pins the fail-closed half of the design:
// a context with no user must produce an error, never a query that
// quietly spans every tenant.
func TestUnscopedContextIsRefused(t *testing.T) {
	pool := testkit.NewDB(t)
	sources := repository.NewSourceRepository(pool)

	// There is something to leak, so an unscoped read that "worked" would
	// return it.
	ctx, _ := testkit.Scoped(t, pool, "+628111000903", "Tenant C")
	_, err := sources.Create(ctx, &entity.Source{
		ID: uuid.New(), Name: "Cash", Type: "cash", Currency: "IDR",
	})
	require.NoError(t, err)

	got, err := sources.List(context.Background(), false)
	require.Error(t, err, "an unscoped read must fail instead of spanning every tenant")
	assert.True(t, errors.Is(err, scope.ErrNoUser), "the refusal must be the missing scope itself, not an incidental SQL error: %v", err)
	assert.Empty(t, got, "a refused read returns no rows")
}
