package service_test

import (
	"context"
	"errors"
	"testing"

	"github.com/glennprays/catet-backend/internal/apperror"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/glennprays/catet-backend/internal/service"
	"github.com/glennprays/catet-backend/internal/testkit"
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
