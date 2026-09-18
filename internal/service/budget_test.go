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

// months returns the YYYY-MM period n months away from the current
// Jakarta month. Anchored on the first of the month so a 31st never
// slides into the month after next.
func months(n int) string {
	now := service.JakartaNow()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	return service.JakartaMonthPeriod(start.AddDate(0, n, 0))
}

// budgetFixture exercises budget segments over a live Postgres: the
// no-overlap exclusion constraint and the date arithmetic are the
// behaviour under test, so nothing here can be judged against a fake.
type budgetFixture struct {
	ctx     context.Context
	budgets *service.BudgetService
	cats    *service.CategoryService
	tx      *service.TransactionService
	src     *service.SourceService
}

func newBudgetFixture(t *testing.T) *budgetFixture {
	t.Helper()
	pool := testkit.NewDB(t)
	ctx, _ := testkit.Scoped(t, pool, "+628111000061", "Budgets")

	categoryRepo := repository.NewCategoryRepository(pool)
	sourceRepo := repository.NewSourceRepository(pool)
	merchantSvc := service.NewMerchantService(repository.NewMerchantRepository(pool), nil, testkit.Logger(t))
	txSvc := service.NewTransactionService(
		repository.NewTransactionRepository(pool), sourceRepo, categoryRepo, merchantSvc,
		repository.NewBillPaymentRepository(pool), repository.NewBillRepository(pool), testkit.Logger(t),
	)
	analyticsSvc := service.NewAnalyticsService(
		repository.NewAnalyticsRepository(pool), categoryRepo, sourceRepo,
		repository.NewBillRepository(pool), repository.NewSettingsRepository(pool),
	)

	return &budgetFixture{
		ctx:     ctx,
		budgets: service.NewBudgetService(repository.NewBudgetRepository(pool), categoryRepo, analyticsSvc),
		cats:    service.NewCategoryService(categoryRepo),
		tx:      txSvc,
		src:     service.NewSourceService(sourceRepo, txSvc),
	}
}

func (f *budgetFixture) category(t *testing.T, name string, parent *uuid.UUID) *entity.Category {
	t.Helper()
	cat, err := f.cats.Create(f.ctx, service.CreateCategoryInput{Name: name, ParentID: parent})
	require.NoError(t, err)
	return cat
}

func (f *budgetFixture) create(t *testing.T, categoryID uuid.UUID, period string, amount int64, recurring bool) *entity.BudgetRow {
	t.Helper()
	row, err := f.budgets.Create(f.ctx, categoryID, period, amount, recurring)
	require.NoError(t, err)
	return row
}

// rows returns the period's budget rows keyed by category name.
func (f *budgetFixture) rows(t *testing.T, period string) map[string]entity.BudgetRow {
	t.Helper()
	_, list, err := f.budgets.List(f.ctx, &period)
	require.NoError(t, err)
	out := make(map[string]entity.BudgetRow, len(list))
	for _, row := range list {
		out[row.CategoryName] = row
	}
	return out
}

func (f *budgetFixture) count(t *testing.T, period string) int {
	t.Helper()
	_, list, err := f.budgets.List(f.ctx, &period)
	require.NoError(t, err)
	return len(list)
}

// TestRecurringBudgetCoversFollowingMonths is the whole point of the
// change: a rule entered once keeps showing up until it is stopped,
// while a one-off stays in its month.
func TestRecurringBudgetCoversFollowingMonths(t *testing.T) {
	f := newBudgetFixture(t)
	makan := f.category(t, "Makan", nil)

	f.create(t, makan.ID, months(-2), 1_200_000, true)

	for _, p := range []string{months(-2), months(0), months(3)} {
		row, ok := f.rows(t, p)["Makan"]
		require.True(t, ok, "recurring budget must cover %s", p)
		assert.Equal(t, int64(1_200_000), row.AmountMinor)
		assert.Equal(t, months(-2), row.FromPeriod)
		assert.Nil(t, row.ToPeriod, "an open rule has no last month")
		assert.Equal(t, months(-2), row.SincePeriod)
	}
	assert.Zero(t, f.count(t, months(-3)), "a rule does not run before it starts")

	jajan := f.category(t, "Jajan", nil)
	f.create(t, jajan.ID, months(0), 300_000, false)
	oneOff := f.rows(t, months(0))["Jajan"]
	require.NotNil(t, oneOff.ToPeriod)
	assert.Equal(t, months(0), *oneOff.ToPeriod)
	assert.NotContains(t, f.rows(t, months(1)), "Jajan")
}

// TestEditFromMonthKeepsPast: raising the limit is effective-dated, so
// last month's comparison stays honest and the new month says what it
// replaced.
func TestEditFromMonthKeepsPast(t *testing.T) {
	f := newBudgetFixture(t)
	makan := f.category(t, "Makan", nil)
	row := f.create(t, makan.ID, months(-2), 1_200_000, true)

	updated, err := f.budgets.UpdateAmount(f.ctx, row.ID, months(0), service.ScopeFromMonth, 1_500_000)
	require.NoError(t, err)
	assert.Equal(t, int64(1_500_000), updated.AmountMinor)

	past := f.rows(t, months(-1))["Makan"]
	assert.Equal(t, int64(1_200_000), past.AmountMinor)
	assert.Nil(t, past.PreviousAmountMinor)

	now := f.rows(t, months(0))["Makan"]
	assert.Equal(t, int64(1_500_000), now.AmountMinor)
	assert.Equal(t, months(0), now.FromPeriod)
	assert.Equal(t, months(-2), now.SincePeriod, "the rule is still the same rule")
	require.NotNil(t, now.PreviousAmountMinor)
	assert.Equal(t, int64(1_200_000), *now.PreviousAmountMinor)

	later := f.rows(t, months(2))["Makan"]
	assert.Equal(t, int64(1_500_000), later.AmountMinor)
	assert.Nil(t, later.PreviousAmountMinor, "only the month of the change carries it")
}

// TestEditOnlyMonthSplitsSegment: a one-month exception leaves the rule
// running on both sides of it.
func TestEditOnlyMonthSplitsSegment(t *testing.T) {
	f := newBudgetFixture(t)
	makan := f.category(t, "Makan", nil)
	row := f.create(t, makan.ID, months(-2), 1_200_000, true)

	_, err := f.budgets.UpdateAmount(f.ctx, row.ID, months(0), service.ScopeOnlyMonth, 2_000_000)
	require.NoError(t, err)

	assert.Equal(t, int64(1_200_000), f.rows(t, months(-1))["Makan"].AmountMinor)

	exception := f.rows(t, months(0))["Makan"]
	assert.Equal(t, int64(2_000_000), exception.AmountMinor)
	require.NotNil(t, exception.ToPeriod)
	assert.Equal(t, months(0), *exception.ToPeriod)

	resumed := f.rows(t, months(1))["Makan"]
	assert.Equal(t, int64(1_200_000), resumed.AmountMinor)
	assert.Nil(t, resumed.ToPeriod, "the rule keeps running after the exception")

	require.NoError(t, f.budgets.Stop(f.ctx, resumed.ID, months(1)))
	assert.Zero(t, f.count(t, months(1)))
	assert.Equal(t, int64(2_000_000), f.rows(t, months(0))["Makan"].AmountMinor)
}

// TestStopAndDeleteAll separates "stop from here" from "this never
// happened".
func TestStopAndDeleteAll(t *testing.T) {
	f := newBudgetFixture(t)
	makan := f.category(t, "Makan", nil)
	row := f.create(t, makan.ID, months(-2), 1_200_000, true)

	require.NoError(t, f.budgets.Stop(f.ctx, row.ID, months(-2)))
	assert.Zero(t, f.count(t, months(-2)), "stopping in the first month removes the rule entirely")

	row = f.create(t, makan.ID, months(-2), 900_000, true)
	require.NoError(t, f.budgets.Stop(f.ctx, row.ID, months(0)))
	assert.Equal(t, 1, f.count(t, months(-1)))
	assert.Zero(t, f.count(t, months(0)))

	history := f.rows(t, months(-1))["Makan"]
	require.NoError(t, f.budgets.DeleteAll(f.ctx, history.ID))
	assert.Zero(t, f.count(t, months(-2)))
	assert.Zero(t, f.count(t, months(-1)))

	err := f.budgets.DeleteAll(f.ctx, history.ID)
	var ae *apperror.Error
	require.True(t, errors.As(err, &ae))
	assert.Equal(t, apperror.KindNotFound, ae.Kind)
}

// TestCreateUnderLaterSegmentTruncates: a new recurring rule stops where
// an already-scheduled one begins instead of failing the overlap check.
func TestCreateUnderLaterSegmentTruncates(t *testing.T) {
	f := newBudgetFixture(t)
	makan := f.category(t, "Makan", nil)

	f.create(t, makan.ID, months(2), 500_000, false)
	row := f.create(t, makan.ID, months(0), 1_200_000, true)
	require.NotNil(t, row.ToPeriod)
	assert.Equal(t, months(1), *row.ToPeriod)

	_, err := f.budgets.Create(f.ctx, makan.ID, months(1), 100_000, true)
	var ae *apperror.Error
	require.True(t, errors.As(err, &ae))
	assert.Equal(t, apperror.KindConflict, ae.Kind)
}

// TestSubtreeSpendAndNested: a parent's bar counts what its children
// spent, and the child row marks itself nested so totals do not
// double-count it.
func TestSubtreeSpendAndNested(t *testing.T) {
	f := newBudgetFixture(t)

	bca, err := f.src.Create(f.ctx, service.CreateSourceInput{Name: "BCA", Type: "bank"})
	require.NoError(t, err)

	makanan := f.category(t, "Makanan", nil)
	jajan := f.category(t, "Jajan", &makanan.ID)

	merchant := "warung"
	now := service.JakartaNow()
	_, err = f.tx.Create(f.ctx, service.CreateInput{
		Type:        "expense",
		OccurredAt:  &now,
		AmountMinor: 30_000,
		SourceID:    bca.Source.ID,
		CategoryID:  &jajan.ID,
		Merchant:    &merchant,
		Origin:      "dashboard_manual",
	})
	require.NoError(t, err)

	period := months(0)
	f.create(t, makanan.ID, period, 1_000_000, true)
	f.create(t, jajan.ID, period, 200_000, true)

	rows := f.rows(t, period)
	assert.Equal(t, int64(30_000), rows["Makanan"].SpentMinor, "the parent counts its subtree")
	assert.Equal(t, int64(30_000), rows["Jajan"].SpentMinor)
	assert.True(t, rows["Jajan"].Nested)
	assert.False(t, rows["Makanan"].Nested)
}

// TestUpdateOutsideCoverage: the dashboard edits the row it is looking
// at; a period the segment does not cover is a client bug, not a write.
func TestUpdateOutsideCoverage(t *testing.T) {
	f := newBudgetFixture(t)
	makan := f.category(t, "Makan", nil)
	row := f.create(t, makan.ID, months(0), 1_200_000, true)

	_, err := f.budgets.UpdateAmount(f.ctx, row.ID, months(-5), service.ScopeFromMonth, 1_500_000)
	var ae *apperror.Error
	require.True(t, errors.As(err, &ae))
	assert.Equal(t, apperror.KindValidation, ae.Kind)
	require.Len(t, ae.Details, 1)
	assert.Equal(t, "period", ae.Details[0].Field)
	assert.Equal(t, "covered", ae.Details[0].Rule)
}
