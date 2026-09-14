package service

import (
	"context"
	"time"

	"github.com/glennprays/catet-backend/internal/apperror"
	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/params/response"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/google/uuid"
)

// The pinned overview formulas (plan §API contract, spec §7), computed
// in Go — the frontend never does money math.
//
//	asset balance       = Σ non-voided legs per source
//	available           = Σ balance of !is_debt sources
//	reserved            = Σ amount_max of active bills due within
//	                      income_horizon_days (conservative band max)
//	sts                 = available − reserved
//	trailing_burn       = Σ expenses over trailing 30 days / 30
//	runway_days         = floor(sts / trailing_burn); 0 when burn ≤ 0 (render ∞)
//	daily_allowance     = floor(sts / income_horizon_days)
//	reserved_tomorrow   = bills due tomorrow (band max)
//
// Integer-safe: sts×30 cannot overflow int64 for realistic balances.

// OverviewInputs is the gathered raw state for the formulas.
type OverviewInputs struct {
	AvailableMinor        int64
	ReservedMinor         int64
	ExpenseSum30Days      int64 // positive magnitude over trailing 30 days
	IncomeHorizonDays     int64 // 1..90
	ReservedTomorrowMinor int64
}

// OverviewOutputs is the derived hero cluster.
type OverviewOutputs struct {
	StsMinor            int64
	RunwayDays          int64 // 0 = burn ≤ 0 → ∞
	DailyAllowanceMinor int64
}

// ComputeOverview is the pure formula core (unit-tested with a fixed
// fixture).
func ComputeOverview(in OverviewInputs) OverviewOutputs {
	sts := in.AvailableMinor - in.ReservedMinor

	var runway int64
	if in.ExpenseSum30Days > 0 {
		// floor(sts / (Σ30/30)) == floor((sts×30)/Σ30) — integer-exact.
		runway = floorDiv(sts*30, in.ExpenseSum30Days)
	}

	return OverviewOutputs{
		StsMinor:            sts,
		RunwayDays:          runway,
		DailyAllowanceMinor: floorDiv(sts, in.IncomeHorizonDays),
	}
}

// floorDiv is mathematical floor division (Go's / truncates toward
// zero, which differs for negative numerators).
func floorDiv(a, b int64) int64 {
	if b == 0 {
		return 0
	}
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

// OverviewService orchestrates the /overview aggregate.
type OverviewService struct {
	sources     *repository.SourceRepository
	bills       *repository.BillRepository
	settings    *repository.SettingsRepository
	analytics   *repository.AnalyticsRepository
	pending     *repository.PendingActionRepository
	transaction *TransactionService
}

func NewOverviewService(
	sources *repository.SourceRepository,
	bills *repository.BillRepository,
	settings *repository.SettingsRepository,
	analytics *repository.AnalyticsRepository,
	pending *repository.PendingActionRepository,
	transaction *TransactionService,
) *OverviewService {
	return &OverviewService{
		sources: sources, bills: bills, settings: settings,
		analytics: analytics, pending: pending, transaction: transaction,
	}
}

// Build assembles the ready-to-render overview snapshot.
func (s *OverviewService) Build(ctx context.Context) (*response.OverviewData, error) {
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
		if src.IsDebt {
			continue
		}
		available += balances[src.ID]
	}

	today, err := parseJakartaDate(JakartaDate())
	if err != nil {
		return nil, apperror.Internal("failed to resolve today").Wrap(err)
	}
	tomorrow := today.AddDate(0, 0, 1)
	reserved, err := s.bills.ReservedSum(ctx, today, today.AddDate(0, 0, horizon))
	if err != nil {
		return nil, apperror.Internal("failed to sum reservations").Wrap(err)
	}
	reservedTomorrow, err := s.bills.ReservedSum(ctx, tomorrow, tomorrow.AddDate(0, 0, 1))
	if err != nil {
		return nil, apperror.Internal("failed to sum tomorrow's reservations").Wrap(err)
	}

	now := time.Now().UTC()
	_, expense30, err := s.analytics.DailyFlows(ctx, now.AddDate(0, 0, -30), now)
	if err != nil {
		return nil, apperror.Internal("failed to sum trailing burn").Wrap(err)
	}
	var expenseSum30 int64
	for _, v := range expense30 {
		expenseSum30 += v
	}

	out := ComputeOverview(OverviewInputs{
		AvailableMinor:        available,
		ReservedMinor:         reserved,
		ExpenseSum30Days:      expenseSum30,
		IncomeHorizonDays:     int64(horizon),
		ReservedTomorrowMinor: reservedTomorrow,
	})

	// Essential/lifestyle split over the current Jakarta month.
	monthStart, monthEnd := currentMonthBounds()
	spendRows, err := s.analytics.CategorySpend(ctx, monthStart, monthEnd)
	if err != nil {
		return nil, apperror.Internal("failed to split essential/lifestyle").Wrap(err)
	}
	var essential, lifestyle int64
	for _, row := range spendRows {
		if row.Essential {
			essential += row.Amount
		} else {
			lifestyle += row.Amount
		}
	}

	// Recent activity: latest 10 non-voided ledger items.
	rows, err := s.transaction.List(ctx, ListInput{Limit: 10})
	if err != nil {
		return nil, err
	}
	recent := make([]response.LedgerItem, 0, len(rows.Rows))
	for _, row := range rows.Rows {
		recent = append(recent, LedgerItemFromRow(row))
	}

	needsReview, err := s.analytics.NeedsReviewCount(ctx)
	if err != nil {
		return nil, apperror.Internal("failed to count needs-review").Wrap(err)
	}
	holdQueue, err := s.pending.CountOpen(ctx)
	if err != nil {
		return nil, apperror.Internal("failed to count hold queue").Wrap(err)
	}

	return &response.OverviewData{
		StsMinor:              out.StsMinor,
		RunwayDays:            out.RunwayDays,
		DailyAllowanceMinor:   out.DailyAllowanceMinor,
		ReservedTomorrowMinor: reservedTomorrow,
		ReservedMinor:         reserved,
		AvailableMinor:        available,
		EssentialMinor:        essential,
		LifestyleMinor:        lifestyle,
		Recent:                recent,
		Badges: response.BadgeCounts{
			NeedsReview: needsReview,
			HoldQueue:   holdQueue,
		},
	}, nil
}

// LedgerItemFromRow maps a joined list row to the response shape.
func LedgerItemFromRow(row repository.LedgerRow) response.LedgerItem {
	return response.LedgerItem{
		TransactionID:         row.TransactionID,
		OccurredAt:            row.OccurredAt,
		Type:                  row.Type,
		Merchant:              row.Merchant,
		Note:                  row.Note,
		AmountMinor:           row.AmountMinor,
		CategoryID:            row.CategoryID,
		CategoryName:          row.CategoryName,
		SourceID:              row.SourceID,
		SourceName:            row.SourceName,
		ReviewStatus:          row.ReviewStatus,
		Voided:                row.Voided,
		ReversesTransactionID: row.ReversesTransactionID,
	}
}

// badgesService is folded into overview — small helper for /badges.
type BadgesService struct {
	analytics *repository.AnalyticsRepository
	pending   *repository.PendingActionRepository
}

func NewBadgesService(analytics *repository.AnalyticsRepository, pending *repository.PendingActionRepository) *BadgesService {
	return &BadgesService{analytics: analytics, pending: pending}
}

func (s *BadgesService) Get(ctx context.Context) (response.BadgeCounts, error) {
	needsReview, err := s.analytics.NeedsReviewCount(ctx)
	if err != nil {
		return response.BadgeCounts{}, apperror.Internal("failed to count needs-review").Wrap(err)
	}
	holdQueue, err := s.pending.CountOpen(ctx)
	if err != nil {
		return response.BadgeCounts{}, apperror.Internal("failed to count hold queue").Wrap(err)
	}
	return response.BadgeCounts{NeedsReview: needsReview, HoldQueue: holdQueue}, nil
}

// settingsService-backed settings handler needs the phone; the config
// value is the single source (env-only per plan).
type SettingsService struct {
	repo  *repository.SettingsRepository
	phone string
}

func NewSettingsService(repo *repository.SettingsRepository, phone string) *SettingsService {
	return &SettingsService{repo: repo, phone: phone}
}

func (s *SettingsService) Get(ctx context.Context) (*response.SettingsData, error) {
	days, err := s.repo.Get(ctx)
	if err != nil {
		return nil, apperror.Internal("failed to load settings").Wrap(err)
	}
	return &response.SettingsData{IncomeHorizonDays: days, Phone: s.phone}, nil
}

func (s *SettingsService) Update(ctx context.Context, days int) (*response.SettingsData, error) {
	if err := s.repo.UpdateIncomeHorizon(ctx, days); err != nil {
		return nil, apperror.Internal("failed to update settings").Wrap(err)
	}
	return &response.SettingsData{IncomeHorizonDays: days, Phone: s.phone}, nil
}

// HoldQueueService resolves parked pending actions from the dashboard
// (ADR-0004).
type HoldQueueService struct {
	repo *repository.PendingActionRepository
}

func NewHoldQueueService(repo *repository.PendingActionRepository) *HoldQueueService {
	return &HoldQueueService{repo: repo}
}

func (s *HoldQueueService) List(ctx context.Context) ([]entity.PendingAction, error) {
	items, err := s.repo.ListOpen(ctx)
	if err != nil {
		return nil, apperror.Internal("failed to list hold queue").Wrap(err)
	}
	return items, nil
}

// Resolve confirms (execute) or discards a parked action. Confirming
// execution of the parked tool call is the bot harness's job — the
// dashboard only marks the decision; today's Now-tier has no parked
// writes beyond the queue row itself.
func (s *HoldQueueService) Resolve(ctx context.Context, id uuid.UUID, action string) error {
	ok, err := s.repo.Resolve(ctx, id, action)
	if err != nil {
		return apperror.Internal("failed to resolve pending action").Wrap(err)
	}
	if !ok {
		return apperror.NotFound("pending action not found")
	}
	return nil
}
