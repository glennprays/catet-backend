package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The pinned overview formulas with a fixed fixture:
//
//	available 10.000.000 · reserved 1.000.000 · expense30 3.000.000
//	horizon 7
//
//	sts             = 9.000.000
//	runway          = floor(9.000.000 / (3.000.000/30)) = 90
//	daily allowance = floor(9.000.000 / 7) = 1.285.714
func TestComputeOverview_FixedFixture(t *testing.T) {
	out := ComputeOverview(OverviewInputs{
		AvailableMinor:    10_000_000,
		ReservedMinor:     1_000_000,
		ExpenseSum30Days:  3_000_000,
		IncomeHorizonDays: 7,
	})
	assert.Equal(t, int64(9_000_000), out.StsMinor)
	assert.Equal(t, int64(90), out.RunwayDays)
	assert.Equal(t, int64(1_285_714), out.DailyAllowanceMinor)
}

// No burn at all (no expenses in the trailing window) → runway 0,
// which the UI renders as ∞.
func TestComputeOverview_ZeroBurnMeansInfiniteRunway(t *testing.T) {
	out := ComputeOverview(OverviewInputs{
		AvailableMinor:    5_000_000,
		ReservedMinor:     0,
		ExpenseSum30Days:  0,
		IncomeHorizonDays: 7,
	})
	assert.Equal(t, int64(5_000_000), out.StsMinor)
	assert.Equal(t, int64(0), out.RunwayDays)
}

// Negative STS floors (not truncates toward zero): sts −99 with burn
// 100/day → runway floor(−0.99) = −1.
func TestComputeOverview_NegativeStsFloors(t *testing.T) {
	out := ComputeOverview(OverviewInputs{
		AvailableMinor:    0,
		ReservedMinor:     99,
		ExpenseSum30Days:  3_000,
		IncomeHorizonDays: 7,
	})
	assert.Equal(t, int64(-99), out.StsMinor)
	assert.Equal(t, int64(-1), out.RunwayDays)
}

// floorDiv is mathematical floor division, unlike Go's truncating /.
func TestFloorDiv(t *testing.T) {
	assert.Equal(t, int64(3), floorDiv(7, 2))
	assert.Equal(t, int64(-4), floorDiv(-7, 2))
	assert.Equal(t, int64(-3), floorDiv(-6, 2))
	assert.Equal(t, int64(0), floorDiv(5, 0))
}
