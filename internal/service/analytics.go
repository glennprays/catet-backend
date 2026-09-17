package service

import (
	"context"
	"math"
	"sort"
	"time"

	"github.com/glennprays/catet-backend/internal/apperror"
	"github.com/glennprays/catet-backend/internal/params/response"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/google/uuid"
)

// AnalyticsService computes the deterministic chart series (spec §7):
// Go returns ready-to-render data; the dashboard only draws.
type AnalyticsService struct {
	analytics *repository.AnalyticsRepository
	cats      *repository.CategoryRepository
	sources   *repository.SourceRepository
	bills     *repository.BillRepository
	settings  *repository.SettingsRepository
}

func NewAnalyticsService(
	analytics *repository.AnalyticsRepository,
	cats *repository.CategoryRepository,
	sources *repository.SourceRepository,
	bills *repository.BillRepository,
	settings *repository.SettingsRepository,
) *AnalyticsService {
	return &AnalyticsService{
		analytics: analytics, cats: cats, sources: sources,
		bills: bills, settings: settings,
	}
}

// ResolveRange parses from/to date params (Jakarta), defaulting to the
// trailing 30 days. Returns [from, to) UTC instants.
func ResolveRange(from, to *string) (time.Time, time.Time, error) {
	today, err := parseJakartaDate(JakartaDate())
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	end := today.AddDate(0, 0, 1)
	start := today.AddDate(0, 0, -29)
	if to != nil && *to != "" {
		if end, err = parseJakartaDate(*to); err != nil {
			return time.Time{}, time.Time{}, apperror.Validation("invalid range").
				WithDetails(apperror.FieldError{Field: "to", Rule: "yyyy-mm-date", Message: "must be YYYY-MM-DD"})
		}
		end = end.AddDate(0, 0, 1) // exclusive end covers the `to` day
	}
	if from != nil && *from != "" {
		if start, err = parseJakartaDate(*from); err != nil {
			return time.Time{}, time.Time{}, apperror.Validation("invalid range").
				WithDetails(apperror.FieldError{Field: "from", Rule: "yyyy-mm-date", Message: "must be YYYY-MM-DD"})
		}
	}
	if !start.Before(end) {
		return time.Time{}, time.Time{}, apperror.Validation("invalid range").
			WithDetails(apperror.FieldError{Field: "from", Rule: "before_to", Message: "from must be before to"})
	}
	return start, end, nil
}

// CategorySpendMap exposes per-category expense totals for budgets.
func (s *AnalyticsService) CategorySpendMap(ctx context.Context, from, to time.Time) (map[uuid.UUID]int64, error) {
	rows, err := s.analytics.CategorySpend(ctx, from, to)
	if err != nil {
		return nil, apperror.Internal("failed to compute category spend").Wrap(err)
	}
	out := make(map[uuid.UUID]int64, len(rows))
	for _, row := range rows {
		out[row.CategoryID] = row.Amount
	}
	return out, nil
}

// CategoryBreakdown builds the top-level donut tree; every parent's
// amount includes its descendants, so children drill-down always
// reconciles with the parent slice.
func (s *AnalyticsService) CategoryBreakdown(ctx context.Context, from, to time.Time) (*response.CategoryBreakdownData, error) {
	rows, err := s.analytics.CategorySpend(ctx, from, to)
	if err != nil {
		return nil, apperror.Internal("failed to compute category spend").Wrap(err)
	}
	// All categories (archived too) so parent chains resolve even when
	// a parent itself has no direct spend.
	all, err := s.cats.List(ctx, true)
	if err != nil {
		return nil, apperror.Internal("failed to list categories").Wrap(err)
	}

	type node struct {
		item   response.CategoryBreakdownNode
		parent *uuid.UUID
		direct int64
	}
	nodes := make(map[uuid.UUID]*node, len(all))
	for _, c := range all {
		nodes[c.ID] = &node{
			item:   response.CategoryBreakdownNode{CategoryID: c.ID, Name: c.Name, Essential: c.Essential},
			parent: c.ParentID,
		}
	}
	var total int64
	for _, row := range rows {
		if n, ok := nodes[row.CategoryID]; ok {
			n.direct = row.Amount
			total += row.Amount
		}
	}

	// Roll descendants into parents (iterative, cycle-safe depth cap).
	for id, n := range nodes {
		amount := n.direct
		cur := n.parent
		for depth := 0; depth < 32 && cur != nil; depth++ {
			amount += nodes[*cur].direct
			cur = nodes[*cur].parent
		}
		nodes[id].item.AmountMinor = amount
	}

	// Link children and collect roots with positive totals.
	children := map[uuid.UUID][]response.CategoryBreakdownNode{}
	roots := make([]response.CategoryBreakdownNode, 0, len(nodes))
	for _, n := range nodes {
		if n.item.AmountMinor <= 0 {
			continue
		}
		n.item.Share = shareOf(n.item.AmountMinor, total)
		if n.parent != nil && nodes[*n.parent] != nil && nodes[*n.parent].item.AmountMinor > 0 {
			children[*n.parent] = append(children[*n.parent], n.item)
		} else {
			roots = append(roots, n.item)
		}
	}
	// Descendants of pruned (zero-total) parents surface at top level
	// via the parent!=nil-but-pruned branch above.

	sort.Slice(roots, func(i, j int) bool { return roots[i].AmountMinor > roots[j].AmountMinor })
	for parentID := range children {
		list := children[parentID]
		sort.Slice(list, func(i, j int) bool { return list[i].AmountMinor > list[j].AmountMinor })
		children[parentID] = list
	}
	// Attach children after sorting (values copied above).
	for i := range roots {
		roots[i].Children = children[roots[i].CategoryID]
	}

	return &response.CategoryBreakdownData{TotalMinor: total, Categories: roots}, nil
}

func shareOf(amount, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return float64(amount) / float64(total)
}

// IncomeExpense buckets daily flows into Mon-start weeks
// (Asia/Jakarta), per flows.md §3.
func (s *AnalyticsService) IncomeExpense(ctx context.Context, from, to time.Time) (*response.IncomeExpenseData, error) {
	income, expense, err := s.analytics.DailyFlows(ctx, from, to)
	if err != nil {
		return nil, apperror.Internal("failed to compute daily flows").Wrap(err)
	}

	type bucket struct {
		income, expense int64
	}
	weeks := map[string]*bucket{}
	var order []string
	for day := from; day.Before(to); day = day.AddDate(0, 0, 1) {
		local := day.In(jakartaLoc)
		// Monday-start offset: (Go weekday Sunday=0 … Saturday=6 + 6) % 7.
		offset := (int(local.Weekday()) + 6) % 7
		weekStart := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, jakartaLoc).AddDate(0, 0, -offset)
		key := weekStart.Format("2006-01-02")
		if _, ok := weeks[key]; !ok {
			weeks[key] = &bucket{}
			order = append(order, key)
		}
		key2 := local.Format("2006-01-02")
		weeks[key].income += income[key2]
		weeks[key].expense += expense[key2]
	}
	sort.Strings(order)

	buckets := make([]response.IncomeExpenseBucket, 0, len(order))
	for _, key := range order {
		b := weeks[key]
		buckets = append(buckets, response.IncomeExpenseBucket{
			WeekStart:    key,
			IncomeMinor:  b.income,
			ExpenseMinor: b.expense,
		})
	}
	return &response.IncomeExpenseData{Buckets: buckets}, nil
}

// stsTrendMaxSpan bounds the per-day runway loop: a year of points is
// already more than a chart can show, and each point walks a 30-day
// window.
const stsTrendMaxSpan = 366 * 24 * time.Hour

// StsTrend plots the now Safe-to-Spend as a flat line with per-day
// runway from the trailing-30d burn ending at that day (spec: STS is a
// now-value, never replayed; burn history moves the curve). The range is
// [from, to) as ResolveRange returns it; the final point matches
// /overview exactly when the range ends today.
func (s *AnalyticsService) StsTrend(ctx context.Context, from, to time.Time) (*response.StsTrendData, error) {
	if to.Sub(from) > stsTrendMaxSpan {
		return nil, apperror.Validation("invalid range").
			WithDetails(apperror.FieldError{Field: "to", Rule: "max_span", Message: "range may not exceed 366 days"})
	}

	// Current STS from the pinned formulas.
	horizon, err := s.settings.Get(ctx)
	if err != nil {
		return nil, apperror.Internal("failed to load settings").Wrap(err)
	}
	sources, err := s.sources.List(ctx, false)
	if err != nil {
		return nil, apperror.Internal("failed to list sources").Wrap(err)
	}
	balances, err := s.sources.Balances(ctx)
	if err != nil {
		return nil, apperror.Internal("failed to compute balances").Wrap(err)
	}
	var available int64
	for _, src := range sources {
		if !src.IsDebt {
			available += balances[src.ID]
		}
	}
	today := JakartaToday()
	activeBills, err := s.bills.ListActive(ctx)
	if err != nil {
		return nil, apperror.Internal("failed to list bills").Wrap(err)
	}
	reserved := ReservedBetween(activeBills, today, today.AddDate(0, 0, horizon), true)
	sts := available - reserved

	// Daily expenses over [from−30, to): each point's runway needs the
	// 30 days of burn ending on it.
	rangeStart := from.AddDate(0, 0, -30)
	_, expense, err := s.analytics.DailyFlows(ctx, rangeStart, to)
	if err != nil {
		return nil, apperror.Internal("failed to compute daily flows").Wrap(err)
	}
	sumBetween := func(from, to time.Time) int64 {
		var sum int64
		for d := from; d.Before(to); d = d.AddDate(0, 0, 1) {
			sum += expense[d.In(jakartaLoc).Format("2006-01-02")]
		}
		return sum
	}

	points := []response.StsPoint{}
	for day := from; day.Before(to); day = day.AddDate(0, 0, 1) {
		windowEnd := day.AddDate(0, 0, 1)
		windowStart := windowEnd.AddDate(0, 0, -30)
		sum30 := sumBetween(windowStart, windowEnd)
		out := ComputeOverview(OverviewInputs{
			AvailableMinor:    available,
			ReservedMinor:     reserved,
			ExpenseSum30Days:  sum30,
			IncomeHorizonDays: int64(horizon),
		})
		points = append(points, response.StsPoint{
			Date:       day.In(jakartaLoc).Format("2006-01-02"),
			StsMinor:   sts,
			RunwayDays: out.RunwayDays,
		})
	}
	return &response.StsTrendData{Points: points}, nil
}

// Heatmap returns per-day outflow with mean/σ and >2σ anomaly flags.
func (s *AnalyticsService) Heatmap(ctx context.Context, from, to time.Time) (*response.HeatmapData, error) {
	_, expense, err := s.analytics.DailyFlows(ctx, from, to)
	if err != nil {
		return nil, apperror.Internal("failed to compute daily flows").Wrap(err)
	}

	var sum int64
	var n int64
	for day := from; day.Before(to); day = day.AddDate(0, 0, 1) {
		sum += expense[day.In(jakartaLoc).Format("2006-01-02")]
		n++
	}
	if n == 0 {
		return &response.HeatmapData{Days: []response.HeatmapDay{}}, nil
	}
	mean := float64(sum) / float64(n)

	var variance float64
	for day := from; day.Before(to); day = day.AddDate(0, 0, 1) {
		v := float64(expense[day.In(jakartaLoc).Format("2006-01-02")])
		variance += (v - mean) * (v - mean)
	}
	variance /= float64(n)
	std := math.Sqrt(variance)

	days := make([]response.HeatmapDay, 0, n)
	for day := from; day.Before(to); day = day.AddDate(0, 0, 1) {
		key := day.In(jakartaLoc).Format("2006-01-02")
		outflow := expense[key]
		anomaly := std > 0 && float64(outflow) > mean+2*std
		days = append(days, response.HeatmapDay{
			Date:         key,
			OutflowMinor: outflow,
			Anomaly:      anomaly,
		})
	}
	return &response.HeatmapData{
		Days:      days,
		MeanMinor: int64(math.Round(mean)),
		StdMinor:  int64(math.Round(std)),
	}, nil
}

// TopMerchants returns the ranked outflow list.
func (s *AnalyticsService) TopMerchants(ctx context.Context, from, to time.Time, limit int) (*response.TopMerchantsData, error) {
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}
	rows, err := s.analytics.TopMerchants(ctx, from, to, limit)
	if err != nil {
		return nil, apperror.Internal("failed to rank merchants").Wrap(err)
	}
	merchants := make([]response.TopMerchant, 0, len(rows))
	for _, row := range rows {
		merchants = append(merchants, response.TopMerchant{
			MerchantID:  row.ID,
			Name:        row.Name,
			AmountMinor: row.AmountMinor,
			TxCount:     row.TxCount,
		})
	}
	return &response.TopMerchantsData{Merchants: merchants}, nil
}

// EssentialLifestyle splits expense by category.essential flags.
func (s *AnalyticsService) EssentialLifestyle(ctx context.Context, from, to time.Time) (*response.EssentialLifestyleData, error) {
	rows, err := s.analytics.CategorySpend(ctx, from, to)
	if err != nil {
		return nil, apperror.Internal("failed to split essential/lifestyle").Wrap(err)
	}
	var essential, lifestyle int64
	for _, row := range rows {
		if row.Essential {
			essential += row.Amount
		} else {
			lifestyle += row.Amount
		}
	}
	return &response.EssentialLifestyleData{EssentialMinor: essential, LifestyleMinor: lifestyle}, nil
}
