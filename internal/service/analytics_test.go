package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/glennprays/catet-backend/internal/apperror"
	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/glennprays/catet-backend/internal/service"
	"github.com/glennprays/catet-backend/internal/testkit"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newAnalyticsFixture(t *testing.T, phone string) (*service.AnalyticsService, context.Context) {
	t.Helper()
	pool := testkit.NewDB(t)
	ctx, _ := testkit.Scoped(t, pool, phone, "Trend")
	svc := service.NewAnalyticsService(
		repository.NewAnalyticsRepository(pool),
		repository.NewCategoryRepository(pool),
		repository.NewSourceRepository(pool),
		repository.NewBillRepository(pool),
		repository.NewSettingsRepository(pool),
	)
	return svc, ctx
}

// categorySpendFixture builds the services a category-spend test needs:
// analytics for the breakdown, transactions to write real expenses, and
// categories to build a parent/child chain.
type categorySpendFixture struct {
	ctx       context.Context
	analytics *service.AnalyticsService
	cats      *service.CategoryService
	src       *service.SourceService
	tx        *service.TransactionService
}

func newCategorySpendFixture(t *testing.T, phone string) *categorySpendFixture {
	t.Helper()
	pool := testkit.NewDB(t)
	ctx, _ := testkit.Scoped(t, pool, phone, "Breakdown")

	categoryRepo := repository.NewCategoryRepository(pool)
	sourceRepo := repository.NewSourceRepository(pool)
	merchantSvc := service.NewMerchantService(repository.NewMerchantRepository(pool), nil, testkit.Logger(t))
	txSvc := service.NewTransactionService(
		repository.NewTransactionRepository(pool), sourceRepo, categoryRepo, merchantSvc,
		repository.NewBillPaymentRepository(pool), repository.NewBillRepository(pool), testkit.Logger(t),
	)
	return &categorySpendFixture{
		ctx: ctx,
		analytics: service.NewAnalyticsService(
			repository.NewAnalyticsRepository(pool), categoryRepo, sourceRepo,
			repository.NewBillRepository(pool), repository.NewSettingsRepository(pool),
		),
		cats: service.NewCategoryService(categoryRepo),
		src:  service.NewSourceService(sourceRepo, txSvc),
		tx:   txSvc,
	}
}

func (f *categorySpendFixture) category(t *testing.T, name string, parent *uuid.UUID) *entity.Category {
	t.Helper()
	cat, err := f.cats.Create(f.ctx, service.CreateCategoryInput{Name: name, ParentID: parent})
	require.NoError(t, err)
	return cat
}

func (f *categorySpendFixture) spend(t *testing.T, sourceID, categoryID uuid.UUID, amount int64) {
	t.Helper()
	now := service.JakartaNow()
	merchant := "warung"
	_, err := f.tx.Create(f.ctx, service.CreateInput{
		Type: "expense", OccurredAt: &now, AmountMinor: amount,
		SourceID: sourceID, CategoryID: &categoryID,
		Merchant: &merchant, Origin: "dashboard_manual",
	})
	require.NoError(t, err)
}

// TestCategoryBreakdownSumsDescendantsIntoParents pins the contract the
// OpenAPI description states ("includes descendants"): a parent's amount
// is its own direct spend plus every descendant's, so drilling into a
// slice always reconciles with the parent. The loop this replaced summed
// each node's ANCESTORS, which made a child larger than its parent.
func TestCategoryBreakdownSumsDescendantsIntoParents(t *testing.T) {
	f := newCategorySpendFixture(t, "+628111000071")

	bca, err := f.src.Create(f.ctx, service.CreateSourceInput{Name: "BCA", Type: "bank"})
	require.NoError(t, err)

	makanan := f.category(t, "Makanan", nil)
	jajan := f.category(t, "Jajan", &makanan.ID)
	permen := f.category(t, "Permen", &jajan.ID)

	f.spend(t, bca.ID, makanan.ID, 100_000)
	f.spend(t, bca.ID, jajan.ID, 30_000)
	f.spend(t, bca.ID, permen.ID, 20_000)

	to := service.JakartaNow().Add(time.Hour)
	from := to.AddDate(0, 0, -30)
	out, err := f.analytics.CategoryBreakdown(f.ctx, from, to)
	require.NoError(t, err)

	require.Len(t, out.Categories, 1, "only Makanan is a root; the chain hangs off it")
	root := out.Categories[0]
	assert.Equal(t, "Makanan", root.Name)
	assert.Equal(t, int64(150_000), root.AmountMinor, "own 100k + child 30k + grandchild 20k")
	require.Len(t, root.Children, 1)
	child := root.Children[0]
	assert.Equal(t, "Jajan", child.Name)
	assert.Equal(t, int64(50_000), child.AmountMinor, "own 30k + grandchild 20k")
	require.Len(t, child.Children, 1)
	assert.Equal(t, "Permen", child.Children[0].Name)
	assert.Equal(t, int64(20_000), child.Children[0].AmountMinor, "a leaf is its own spend")
	assert.Equal(t, int64(150_000), out.TotalMinor, "each rupiah counts once")
	assert.InDelta(t, 1.0, root.Share, 1e-9)
}

// TestTransactionListCategoryIncludesDescendants pins the dashboard's
// parent drill: filtering by a parent with include_descendants shows the
// whole subtree, so the row count agrees with the subtree amount the
// Analytics donut showed for that category.
func TestTransactionListCategoryIncludesDescendants(t *testing.T) {
	f := newCategorySpendFixture(t, "+628111000072")

	bca, err := f.src.Create(f.ctx, service.CreateSourceInput{Name: "BCA", Type: "bank"})
	require.NoError(t, err)

	makanan := f.category(t, "Makanan", nil)
	jajan := f.category(t, "Jajan", &makanan.ID)
	permen := f.category(t, "Permen", &jajan.ID)

	f.spend(t, bca.ID, makanan.ID, 100_000)
	f.spend(t, bca.ID, jajan.ID, 30_000)
	f.spend(t, bca.ID, permen.ID, 20_000)

	exact, err := f.tx.List(f.ctx, service.ListInput{CategoryID: &makanan.ID, Limit: 50})
	require.NoError(t, err)
	assert.Equal(t, int64(1), exact.Total, "exact match is the pre-existing behaviour")

	subtree, err := f.tx.List(f.ctx, service.ListInput{
		CategoryID: &makanan.ID, IncludeDescendants: true, Limit: 50,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(3), subtree.Total, "the parent filter counts its whole subtree")
}

// TestStsTrendCoversTheRequestedRange pins the from/to contract the
// Analytics period picker relies on: one point per day of the asked-for
// window, in order, whatever "today" happens to be.
func TestStsTrendCoversTheRequestedRange(t *testing.T) {
	svc, ctx := newAnalyticsFixture(t, "+628111000005")

	// "Bulan lalu" for a 31-day month: 31 points, first and last named.
	from := "2026-08-01"
	to := "2026-08-31"
	start, end, err := service.ResolveRange(&from, &to)
	require.NoError(t, err)

	out, err := svc.StsTrend(ctx, start, end)
	require.NoError(t, err)
	require.Len(t, out.Points, 31)
	assert.Equal(t, "2026-08-01", out.Points[0].Date)
	assert.Equal(t, "2026-08-31", out.Points[30].Date)
}

// TestStsTrendRejectsAnOverlongRange keeps the per-day loop bounded: each
// point walks its own 30-day window, so an unbounded span is a slow query
// disguised as a chart.
func TestStsTrendRejectsAnOverlongRange(t *testing.T) {
	svc, ctx := newAnalyticsFixture(t, "+628111000006")

	from := "2020-01-01"
	to := "2026-01-01"
	start, end, err := service.ResolveRange(&from, &to)
	require.NoError(t, err)

	_, err = svc.StsTrend(ctx, start, end)
	require.Error(t, err)
	var ae *apperror.Error
	require.True(t, errors.As(err, &ae))
	assert.Equal(t, apperror.KindValidation, ae.Kind)
	require.Len(t, ae.Details, 1)
	assert.Equal(t, "max_span", ae.Details[0].Rule)
}
