package service_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/glennprays/catet-backend/internal/params/response"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/glennprays/catet-backend/internal/service"
	"github.com/glennprays/catet-backend/internal/testkit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mustJSON encodes exactly what the handler writes on the wire, so the
// assertions below read the same bytes the dashboard parses.
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err, "encode response envelope")
	return string(b)
}

// TestEmptyAccountListsAreArraysNotNull pins the shape a freshly
// registered account reads. The OpenAPI contract marks every list field
// required, so the generated dashboard client types them non-nullable
// and index them (`data.categories.length`) without a null guard: a nil
// Go slice serialised as `null` crashes the page, it does not degrade.
func TestEmptyAccountListsAreArraysNotNull(t *testing.T) {
	pool := testkit.NewDB(t)
	ctx, _ := testkit.Scoped(t, pool, "+628111000003", "Empty")

	sourceRepo := repository.NewSourceRepository(pool)
	transactionRepo := repository.NewTransactionRepository(pool)
	categoryRepo := repository.NewCategoryRepository(pool)
	billRepo := repository.NewBillRepository(pool)
	budgetRepo := repository.NewBudgetRepository(pool)
	settingsRepo := repository.NewSettingsRepository(pool)
	pendingRepo := repository.NewPendingActionRepository(pool)
	analyticsRepo := repository.NewAnalyticsRepository(pool)
	billPaymentRepo := repository.NewBillPaymentRepository(pool)

	analyticsSvc := service.NewAnalyticsService(analyticsRepo, categoryRepo, sourceRepo, billRepo, settingsRepo)
	txSvc := service.NewTransactionService(transactionRepo, sourceRepo, categoryRepo, billPaymentRepo, billRepo, testkit.Logger(t))
	overviewSvc := service.NewOverviewService(sourceRepo, billRepo, settingsRepo, analyticsRepo, pendingRepo, txSvc)
	sourceSvc := service.NewSourceService(sourceRepo, txSvc)
	billSvc := service.NewBillService(billRepo, billPaymentRepo, txSvc, sourceRepo)
	budgetSvc := service.NewBudgetService(budgetRepo, categoryRepo, analyticsSvc)

	to := time.Now().UTC()
	from := to.AddDate(0, 0, -30)

	t.Run("category breakdown", func(t *testing.T) {
		out, err := analyticsSvc.CategoryBreakdown(ctx, from, to)
		require.NoError(t, err)
		body := mustJSON(t, out)
		assert.Contains(t, body, `"categories":[]`,
			"a null here is what crashed /analytics: the chart reads categories.length with no guard")
	})

	t.Run("income vs expense", func(t *testing.T) {
		out, err := analyticsSvc.IncomeExpense(ctx, from, to)
		require.NoError(t, err)
		body := mustJSON(t, out)
		assert.NotContains(t, body, `:null`,
			"buckets must stay an array; null would break the timeline chart's .map")
	})

	t.Run("sts trend", func(t *testing.T) {
		out, err := analyticsSvc.StsTrend(ctx, 30)
		require.NoError(t, err)
		body := mustJSON(t, out)
		assert.NotContains(t, body, `:null`,
			"points must stay an array; null would break the trend chart's .length")
	})

	t.Run("heatmap", func(t *testing.T) {
		out, err := analyticsSvc.Heatmap(ctx, from, to)
		require.NoError(t, err)
		body := mustJSON(t, out)
		assert.NotContains(t, body, `:null`,
			"days must stay an array; null would break the heatmap's per-day .map")
	})

	t.Run("top merchants", func(t *testing.T) {
		out, err := analyticsSvc.TopMerchants(ctx, from, to, 10)
		require.NoError(t, err)
		body := mustJSON(t, out)
		assert.Contains(t, body, `"merchants":[]`,
			"the merchants table reads .length to pick its empty state; null throws instead")
	})

	t.Run("overview", func(t *testing.T) {
		out, err := overviewSvc.Build(ctx)
		require.NoError(t, err)
		body := mustJSON(t, out)
		assert.Contains(t, body, `"recent":[]`,
			"the overview's recent list is mapped straight to rows; null throws on the home page")
	})

	t.Run("sources", func(t *testing.T) {
		out, err := sourceSvc.List(ctx, false)
		require.NoError(t, err)
		body := mustJSON(t, out)
		assert.Contains(t, body, `"sources":[]`,
			"/sources reads sources.length before rendering; null throws")
	})

	t.Run("budgets", func(t *testing.T) {
		period := "2026-09"
		p, rows, err := budgetSvc.List(ctx, &period)
		require.NoError(t, err)
		body := mustJSON(t, response.BudgetsData{Period: p, Budgets: rows})
		assert.Contains(t, body, `"budgets":[]`,
			"/budgets reads budgets.length before rendering; null throws")
	})

	t.Run("bills", func(t *testing.T) {
		bills, err := billRepo.List(ctx)
		require.NoError(t, err)
		body := mustJSON(t, response.BillsData{Bills: bills})
		assert.Contains(t, body, `"bills":[]`,
			"/bills maps bills into the calendar; null throws")
	})

	t.Run("bill reminder offsets", func(t *testing.T) {
		due := service.JakartaToday().Format("2006-01-02")
		bill, err := billSvc.Create(ctx, service.CreateBillInput{
			Name: "Listrik", AmountMinMinor: 200_000, AmountMaxMinor: 250_000,
			Cadence: "monthly", NextDueDate: due,
		})
		require.NoError(t, err)
		body := mustJSON(t, bill)
		assert.Contains(t, body, `"reminder_offsets":[]`,
			"the bill form ticks checkboxes against this array; null throws before it renders")
	})

	t.Run("bill payments", func(t *testing.T) {
		today := service.JakartaToday()
		payments, err := billSvc.Payments(ctx,
			today.Format("2006-01-02"), today.AddDate(0, 0, 1).Format("2006-01-02"))
		require.NoError(t, err)
		body := mustJSON(t, response.BillPaymentsData{Payments: payments})
		assert.Contains(t, body, `"payments":[]`,
			"the calendar marks paid days from this array; null throws")
	})

	t.Run("hold queue", func(t *testing.T) {
		items, err := pendingRepo.ListOpen(ctx)
		require.NoError(t, err)
		body := mustJSON(t, response.HoldQueueData{Items: items})
		assert.Contains(t, body, `"items":[]`,
			"the hold queue reads items.length for its badge and empty state; null throws")
	})

	t.Run("categories", func(t *testing.T) {
		cats, err := categoryRepo.List(ctx, false)
		require.NoError(t, err)
		body := mustJSON(t, response.CategoriesData{Categories: cats})
		assert.Contains(t, body, `"categories":[]`,
			"/categories maps the list directly; null throws")
	})
}
