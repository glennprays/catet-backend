package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/glennprays/catet-backend/internal/apperror"
	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/glennprays/catet-backend/internal/service"
	"github.com/glennprays/catet-backend/internal/testkit"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

// ledgerFixture wires the real repositories over a live Postgres.
type ledgerFixture struct {
	ctx        context.Context
	tx         *service.TransactionService
	src        *service.SourceService
	sourceRepo *repository.SourceRepository
	catRepo    *repository.CategoryRepository
	analytics  *repository.AnalyticsRepository
}

// The ledger is per-user, so the fixture opens an account first: every
// repository below reads its tenant from this context, and a bare one
// would fail closed.
func newLedgerFixture(t *testing.T) *ledgerFixture {
	t.Helper()
	pool := testkit.NewDB(t)
	ctx, _ := testkit.Scoped(t, pool, "+628111000001", "Ledger")
	sourceRepo := repository.NewSourceRepository(pool)
	categoryRepo := repository.NewCategoryRepository(pool)
	transactionRepo := repository.NewTransactionRepository(pool)
	merchantSvc := service.NewMerchantService(repository.NewMerchantRepository(pool), nil, testkit.Logger(t))
	txSvc := service.NewTransactionService(
		transactionRepo, sourceRepo, categoryRepo, merchantSvc,
		repository.NewBillPaymentRepository(pool), repository.NewBillRepository(pool), testkit.Logger(t),
	)
	return &ledgerFixture{
		ctx:        ctx,
		tx:         txSvc,
		src:        service.NewSourceService(sourceRepo, txSvc),
		sourceRepo: sourceRepo,
		catRepo:    categoryRepo,
		analytics:  repository.NewAnalyticsRepository(pool),
	}
}

func (f *ledgerFixture) createSource(t *testing.T, name, typ string, isDebt bool) *entity.Source {
	t.Helper()
	out, err := f.src.Create(f.ctx, service.CreateSourceInput{Name: name, Type: typ, IsDebt: isDebt})
	require.NoError(t, err)
	return &out.Source
}

func (f *ledgerFixture) createCategory(t *testing.T, name string) *entity.Category {
	t.Helper()
	cat, err := f.catRepo.Create(f.ctx, &entity.Category{ID: uuid.New(), Name: name, Essential: false})
	require.NoError(t, err)
	return cat
}

func (f *ledgerFixture) balances(t *testing.T) map[uuid.UUID]int64 {
	t.Helper()
	b, err := f.sourceRepo.Balances(f.ctx)
	require.NoError(t, err)
	return b
}

func (f *ledgerFixture) adjust(t *testing.T, sourceID uuid.UUID, amount int64, note string) {
	t.Helper()
	_, err := f.tx.Adjust(f.ctx, sourceID, amount, &note)
	require.NoError(t, err)
}

func (f *ledgerFixture) createExpense(t *testing.T, sourceID, categoryID uuid.UUID, amount int64, merchant string) *entity.Transaction {
	t.Helper()
	tx, err := f.tx.Create(f.ctx, service.CreateInput{
		Type:        "expense",
		OccurredAt:  timePtr(time.Now()),
		AmountMinor: amount,
		SourceID:    sourceID,
		CategoryID:  &categoryID,
		Merchant:    &merchant,
		Origin:      "dashboard_manual",
	})
	require.NoError(t, err)
	return tx
}

func timePtr(t time.Time) *time.Time { return &t }

// TestTransferMirrorsLegs verifies the ADR-0001 transfer shape and the
// end-to-end balance story from the plan's verification scenario:
// top up BCA, transfer BCA→GoPay, then both balances move.
func TestTransferMirrorsLegs(t *testing.T) {
	f := newLedgerFixture(t)

	bca := f.createSource(t, "BCA", "bank", false)
	gopay := f.createSource(t, "GoPay", "ewallet", false)
	f.adjust(t, bca.ID, 2_000_000, "saldo awal")

	transfer, err := f.tx.Create(f.ctx, service.CreateInput{
		Type:                "transfer",
		OccurredAt:          timePtr(time.Now()),
		AmountMinor:         500_000,
		SourceID:            bca.ID,
		DestinationSourceID: &gopay.ID,
		Origin:              "dashboard_manual",
	})
	require.NoError(t, err)

	// Two source legs, mirrored signs; no category legs.
	require.Len(t, transfer.Legs, 2)
	assert.Equal(t, -int64(500_000), transfer.Legs[0].SignedAmount)
	require.NotNil(t, transfer.Legs[0].SourceID)
	assert.Equal(t, bca.ID, *transfer.Legs[0].SourceID)
	assert.Equal(t, int64(500_000), transfer.Legs[1].SignedAmount)
	require.NotNil(t, transfer.Legs[1].SourceID)
	assert.Equal(t, gopay.ID, *transfer.Legs[1].SourceID)
	for _, leg := range transfer.Legs {
		assert.Nil(t, leg.CategoryID)
	}
	assert.Equal(t, int64(500_000), transfer.AmountMinor)
	assert.Equal(t, "dashboard_manual", transfer.Origin)

	balances := f.balances(t)
	assert.Equal(t, int64(1_500_000), balances[bca.ID])
	assert.Equal(t, int64(500_000), balances[gopay.ID])
}

// TestVoidExcludesFromBalances verifies voiding removes a transaction
// from all math and from the default list, while the Di-void bin still
// reaches it.
func TestVoidExcludesFromBalances(t *testing.T) {
	f := newLedgerFixture(t)

	gopay := f.createSource(t, "GoPay", "ewallet", false)
	f.adjust(t, gopay.ID, 500_000, "saldo awal")
	cat := f.createCategory(t, "Makanan")

	kopi := f.createExpense(t, gopay.ID, cat.ID, 25_000, "kopi")
	teh := f.createExpense(t, gopay.ID, cat.ID, 10_000, "teh")
	assert.Equal(t, int64(465_000), f.balances(t)[gopay.ID])

	require.NoError(t, f.tx.Void(f.ctx, kopi.ID))

	assert.Equal(t, int64(490_000), f.balances(t)[gopay.ID])

	// Gone from the default list (filter to expenses; the fixture's
	// adjustment row is not under test).
	dir := "expense"
	page, err := f.tx.List(f.ctx, service.ListInput{Limit: 10, Direction: &dir})
	require.NoError(t, err)
	require.Len(t, page.Rows, 1)
	assert.Equal(t, teh.ID, page.Rows[0].TransactionID)
	assert.Equal(t, int64(1), page.Total)

	// The Di-void bin holds exactly it.
	only := "only"
	page, err = f.tx.List(f.ctx, service.ListInput{Limit: 10, Direction: &dir, Voided: &only})
	require.NoError(t, err)
	require.Len(t, page.Rows, 1)
	assert.Equal(t, kopi.ID, page.Rows[0].TransactionID)
	assert.True(t, page.Rows[0].Voided)

	// include shows both.
	include := "include"
	page, err = f.tx.List(f.ctx, service.ListInput{Limit: 10, Direction: &dir, Voided: &include})
	require.NoError(t, err)
	assert.Len(t, page.Rows, 2)

	// Editing a voided transaction is rejected.
	note := "x"
	_, err = f.tx.Update(f.ctx, kopi.ID, service.UpdateInput{Note: &note})
	require.Error(t, err)
	var ae *apperror.Error
	require.True(t, errors.As(err, &ae))
	assert.Equal(t, apperror.KindConflict, ae.Kind)
}

// TestVoidedFilterRejectsUnknownValue keeps the enum honest: a typo is a
// validation error naming the field, never a silent full-history list.
func TestVoidedFilterRejectsUnknownValue(t *testing.T) {
	f := newLedgerFixture(t)

	maybe := "maybe"
	_, err := f.tx.List(f.ctx, service.ListInput{Limit: 10, Voided: &maybe})
	require.Error(t, err)
	var ae *apperror.Error
	require.True(t, errors.As(err, &ae))
	assert.Equal(t, apperror.KindValidation, ae.Kind)
	require.Len(t, ae.Details, 1)
	assert.Equal(t, "voided", ae.Details[0].Field)
}

// TestReversedOriginalLeavesTheReviewQueue: a cancelled capture is not a
// question. The original keeps its needs_review status (state is pure)
// but leaves the queue list and the badge until the reversal is undone.
func TestReversedOriginalLeavesTheReviewQueue(t *testing.T) {
	f := newLedgerFixture(t)

	bca := f.createSource(t, "BCA", "bank", false)
	f.adjust(t, bca.ID, 500_000, "saldo awal")

	merchant := "kopi kenangan"
	original, err := f.tx.Create(f.ctx, service.CreateInput{
		Type:        "expense",
		OccurredAt:  timePtr(time.Now()),
		AmountMinor: 25_000,
		SourceID:    bca.ID,
		Merchant:    &merchant,
		Origin:      "whatsapp_text",
	})
	require.NoError(t, err)
	require.Equal(t, "needs_review", original.ReviewStatus)

	needsReview := "needs_review"
	page, err := f.tx.List(f.ctx, service.ListInput{Limit: 10, ReviewStatus: &needsReview})
	require.NoError(t, err)
	require.Len(t, page.Rows, 1)
	n, err := f.analytics.NeedsReviewCount(f.ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)

	reversal, err := f.tx.Reverse(f.ctx, original.ID, nil, "whatsapp_text")
	require.NoError(t, err)

	page, err = f.tx.List(f.ctx, service.ListInput{Limit: 10, ReviewStatus: &needsReview})
	require.NoError(t, err)
	assert.Empty(t, page.Rows)
	assert.Equal(t, int64(0), page.Total)
	n, err = f.analytics.NeedsReviewCount(f.ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)

	// Both rows stay visible in the plain ledger.
	dir := "expense"
	page, err = f.tx.List(f.ctx, service.ListInput{Limit: 10, Direction: &dir})
	require.NoError(t, err)
	assert.Len(t, page.Rows, 2)

	// Undo the undo: the question is open again.
	require.NoError(t, f.tx.Void(f.ctx, reversal.ID))
	page, err = f.tx.List(f.ctx, service.ListInput{Limit: 10, ReviewStatus: &needsReview})
	require.NoError(t, err)
	require.Len(t, page.Rows, 1)
	assert.Equal(t, original.ID, page.Rows[0].TransactionID)
	n, err = f.analytics.NeedsReviewCount(f.ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
}

// TestReversalLinkage verifies a reversal mirrors legs, links back to
// the original, and cancels its balance effect.
func TestReversalLinkage(t *testing.T) {
	f := newLedgerFixture(t)

	gopay := f.createSource(t, "GoPay", "ewallet", false)
	f.adjust(t, gopay.ID, 500_000, "saldo awal")
	cat := f.createCategory(t, "Transportasi")

	gojek := f.createExpense(t, gopay.ID, cat.ID, 30_000, "gojek")
	assert.Equal(t, int64(470_000), f.balances(t)[gopay.ID])

	note := "refund gojek"
	reversal, err := f.tx.Reverse(f.ctx, gojek.ID, &note, "dashboard_manual")
	require.NoError(t, err)

	require.NotNil(t, reversal.ReversesTransactionID)
	assert.Equal(t, gojek.ID, *reversal.ReversesTransactionID)
	// Mirrored legs, ordered by signed amount ASC: the category leg is
	// now −30.000 and the source leg +30.000.
	assert.Equal(t, int64(-30_000), reversal.Legs[0].SignedAmount)
	require.NotNil(t, reversal.Legs[0].CategoryID)
	assert.Equal(t, cat.ID, *reversal.Legs[0].CategoryID)
	assert.Equal(t, int64(30_000), reversal.Legs[1].SignedAmount)
	require.NotNil(t, reversal.Legs[1].SourceID)
	assert.Equal(t, gopay.ID, *reversal.Legs[1].SourceID)

	// Balance restored; both transactions stay visible.
	assert.Equal(t, int64(500_000), f.balances(t)[gopay.ID])
	dir := "expense"
	page, err := f.tx.List(f.ctx, service.ListInput{Limit: 10, Direction: &dir})
	require.NoError(t, err)
	require.Len(t, page.Rows, 2)
	// Reversing a reversal is rejected.
	_, err = f.tx.Reverse(f.ctx, reversal.ID, nil, "dashboard_manual")
	require.Error(t, err)
}

// TestEditHistoryAndLegRewrite verifies PATCH rewrites legs and
// appends an edit_history entry.
func TestEditHistoryAndLegRewrite(t *testing.T) {
	f := newLedgerFixture(t)

	gopay := f.createSource(t, "GoPay", "ewallet", false)
	f.adjust(t, gopay.ID, 500_000, "saldo awal")
	makanan := f.createCategory(t, "Makanan")
	transport := f.createCategory(t, "Transportasi")

	kopi := f.createExpense(t, gopay.ID, makanan.ID, 25_000, "kopi")

	newAmount := int64(35_000)
	updated, err := f.tx.Update(f.ctx, kopi.ID, service.UpdateInput{AmountMinor: &newAmount, CategoryID: &transport.ID})
	require.NoError(t, err)

	assert.Equal(t, int64(35_000), updated.AmountMinor)
	require.NotNil(t, updated.CategoryID)
	assert.Equal(t, transport.ID, *updated.CategoryID)
	require.Len(t, updated.Legs, 2)
	assert.Equal(t, -int64(35_000), updated.Legs[0].SignedAmount)
	assert.Equal(t, int64(465_000), f.balances(t)[gopay.ID])
	var history []map[string]any
	require.NoError(t, json.Unmarshal(updated.EditHistory, &history))
	require.Len(t, history, 1)
	assert.Contains(t, history[0]["changes"], "amount_minor")
}

// TestCategorizeUncategorizedExpense verifies the correction path the
// bot needs: an expense the harness wrote without a category (parked in
// Needs-Review) can be labelled afterwards, and the label counts as the
// user's.
func TestCategorizeUncategorizedExpense(t *testing.T) {
	f := newLedgerFixture(t)

	bca := f.createSource(t, "BCA", "bank", false)
	f.adjust(t, bca.ID, 500_000, "saldo awal")
	makanan := f.createCategory(t, "Makanan & Minuman")

	merchant := "ketang goreng"
	tx, err := f.tx.Create(f.ctx, service.CreateInput{
		Type:        "expense",
		OccurredAt:  timePtr(time.Now()),
		AmountMinor: 10_000,
		SourceID:    bca.ID,
		Merchant:    &merchant,
		Origin:      "whatsapp_text",
	})
	require.NoError(t, err)
	require.Nil(t, tx.CategoryID)
	assert.Equal(t, "needs_review", tx.ReviewStatus)

	updated, err := f.tx.Update(f.ctx, tx.ID, service.UpdateInput{CategoryID: &makanan.ID})
	require.NoError(t, err)

	require.NotNil(t, updated.CategoryID)
	assert.Equal(t, makanan.ID, *updated.CategoryID)
	require.NotNil(t, updated.CategorySource)
	assert.Equal(t, "user", *updated.CategorySource)
	assert.Equal(t, "clean", updated.ReviewStatus)

	// Legs keep the source and gain the category side.
	require.Len(t, updated.Legs, 2)
	assert.Equal(t, -int64(10_000), updated.Legs[0].SignedAmount)
	require.NotNil(t, updated.Legs[0].SourceID)
	assert.Equal(t, bca.ID, *updated.Legs[0].SourceID)
	assert.Equal(t, int64(10_000), updated.Legs[1].SignedAmount)
	require.NotNil(t, updated.Legs[1].CategoryID)
	assert.Equal(t, makanan.ID, *updated.Legs[1].CategoryID)
	assert.Equal(t, int64(490_000), f.balances(t)[bca.ID])
}
