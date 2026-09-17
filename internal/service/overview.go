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
	activeBills, err := s.bills.ListActive(ctx)
	if err != nil {
		return nil, apperror.Internal("failed to list bills").Wrap(err)
	}
	// Every occurrence inside the horizon is reserved, not just the next
	// one: a weekly bill costs four times before payday. Overdue bills
	// keep their reservation — money still owed is not safe to spend.
	reserved := ReservedBetween(activeBills, today, today.AddDate(0, 0, horizon), true)
	reservedTomorrow := ReservedBetween(activeBills, tomorrow, tomorrow.AddDate(0, 0, 1), false)

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
	billsOverdue, err := s.bills.CountOverdue(ctx, today)
	if err != nil {
		return nil, apperror.Internal("failed to count overdue bills").Wrap(err)
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
			NeedsReview:  needsReview,
			HoldQueue:    holdQueue,
			BillsOverdue: billsOverdue,
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
		MerchantID:            row.MerchantID,
		Note:                  row.Note,
		AmountMinor:           row.AmountMinor,
		CategoryID:            row.CategoryID,
		CategoryName:          row.CategoryName,
		SourceID:              row.SourceID,
		SourceName:            row.SourceName,
		ReviewStatus:          row.ReviewStatus,
		CategorySource:        row.CategorySource,
		CategoryConfidence:    row.CategoryConfidence,
		Voided:                row.Voided,
		ReversesTransactionID: row.ReversesTransactionID,
	}
}

// badgesService is folded into overview — small helper for /badges.
type BadgesService struct {
	analytics *repository.AnalyticsRepository
	pending   *repository.PendingActionRepository
	bills     *repository.BillRepository
}

func NewBadgesService(
	analytics *repository.AnalyticsRepository,
	pending *repository.PendingActionRepository,
	bills *repository.BillRepository,
) *BadgesService {
	return &BadgesService{analytics: analytics, pending: pending, bills: bills}
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
	today, err := parseJakartaDate(JakartaDate())
	if err != nil {
		return response.BadgeCounts{}, apperror.Internal("failed to resolve today").Wrap(err)
	}
	billsOverdue, err := s.bills.CountOverdue(ctx, today)
	if err != nil {
		return response.BadgeCounts{}, apperror.Internal("failed to count overdue bills").Wrap(err)
	}
	return response.BadgeCounts{NeedsReview: needsReview, HoldQueue: holdQueue, BillsOverdue: billsOverdue}, nil
}

// SettingsService backs the settings handler. The Source repository is
// here to turn the stored receipt-fallback id into a name the dashboard
// can show, and to refuse an id that is not a live Source. The account
// itself is not a setting: the dashboard reads the phone and display
// name from /auth/me.
type SettingsService struct {
	repo    *repository.SettingsRepository
	sources *repository.SourceRepository
}

func NewSettingsService(repo *repository.SettingsRepository, sources *repository.SourceRepository) *SettingsService {
	return &SettingsService{repo: repo, sources: sources}
}

func (s *SettingsService) Get(ctx context.Context) (*response.SettingsData, error) {
	days, err := s.repo.Get(ctx)
	if err != nil {
		return nil, apperror.Internal("failed to load settings").Wrap(err)
	}
	photoSourceID, err := s.repo.GetPhotoDefaultSource(ctx)
	if err != nil {
		return nil, apperror.Internal("failed to load settings").Wrap(err)
	}
	name, err := s.photoDefaultSourceName(ctx, photoSourceID)
	if err != nil {
		return nil, err
	}
	hour, err := s.repo.GetReminderHour(ctx)
	if err != nil {
		return nil, apperror.Internal("failed to load settings").Wrap(err)
	}
	return &response.SettingsData{
		IncomeHorizonDays:      days,
		PhotoDefaultSourceID:   photoSourceID,
		PhotoDefaultSourceName: name,
		ReminderHour:           hour,
	}, nil
}

// Update writes the settings form. reminderHour is optional: a client
// that predates the field leaves the digest hour alone instead of
// silently resetting it to 08:00.
func (s *SettingsService) Update(ctx context.Context, days int, photoDefaultSourceID *uuid.UUID, reminderHour *int) (*response.SettingsData, error) {
	name, err := s.photoDefaultSourceName(ctx, photoDefaultSourceID)
	if err != nil {
		return nil, err
	}
	if err := s.repo.UpdateIncomeHorizon(ctx, days); err != nil {
		return nil, apperror.Internal("failed to update settings").Wrap(err)
	}
	if err := s.repo.SetPhotoDefaultSource(ctx, photoDefaultSourceID); err != nil {
		return nil, apperror.Internal("failed to update settings").Wrap(err)
	}
	if reminderHour != nil {
		if err := s.repo.SetReminderHour(ctx, *reminderHour); err != nil {
			return nil, apperror.Internal("failed to update settings").Wrap(err)
		}
	}
	hour, err := s.repo.GetReminderHour(ctx)
	if err != nil {
		return nil, apperror.Internal("failed to load settings").Wrap(err)
	}
	return &response.SettingsData{
		IncomeHorizonDays:      days,
		PhotoDefaultSourceID:   photoDefaultSourceID,
		PhotoDefaultSourceName: name,
		ReminderHour:           hour,
	}, nil
}

// photoDefaultSourceName resolves the fallback id against the active
// Sources. An archived or unknown Source is rejected rather than stored
// silently: the bot would otherwise pay receipts from an account the
// user has retired.
func (s *SettingsService) photoDefaultSourceName(ctx context.Context, id *uuid.UUID) (*string, error) {
	if id == nil {
		return nil, nil
	}
	sources, err := s.sources.List(ctx, false)
	if err != nil {
		return nil, apperror.Internal("failed to load sources").Wrap(err)
	}
	for i := range sources {
		if sources[i].ID == *id {
			name := sources[i].Name
			return &name, nil
		}
	}
	return nil, apperror.Validation("settings validation failed").
		WithDetails(apperror.FieldError{Field: "photo_default_source_id", Rule: "exists", Message: "source not found"})
}

// PendingOverrides are the edits a confirming surface may apply to a
// parked draft before it is written. Nil pointer = keep what the bot
// stored.
type PendingOverrides struct {
	AmountMinor         *int64
	SourceID            *uuid.UUID
	DestinationSourceID *uuid.UUID
	CategoryID          *uuid.UUID
	Merchant            *string
	Note                *string
	OccurredDate        *string
	// Collapse writes a receipt as one line at its printed total instead
	// of one line per item.
	Collapse *bool
	// Items are per-line corrections for a receipt, keyed by the parked
	// line's N.
	Items []PendingItemOverride
}

// PendingItemOverride is one receipt line's correction: the line's
// parked N and the Category the user chose for it.
type PendingItemOverride struct {
	N          int
	CategoryID *uuid.UUID
}

// PendingExecutor performs a confirmed pending action and returns every
// transaction it wrote — one for a capture or refund, one per line for a
// photographed receipt. Implemented by the bot harness; declared here so
// the service layer does not depend on the bot package.
type PendingExecutor interface {
	Execute(ctx context.Context, p *entity.PendingAction, ov *PendingOverrides) ([]*entity.Transaction, error)
}

// HoldQueueService resolves parked pending actions from the dashboard
// (ADR-0004).
type HoldQueueService struct {
	repo     *repository.PendingActionRepository
	executor PendingExecutor
}

func NewHoldQueueService(repo *repository.PendingActionRepository, executor PendingExecutor) *HoldQueueService {
	return &HoldQueueService{repo: repo, executor: executor}
}

func (s *HoldQueueService) List(ctx context.Context) ([]entity.PendingAction, error) {
	items, err := s.repo.ListOpen(ctx)
	if err != nil {
		return nil, apperror.Internal("failed to list hold queue").Wrap(err)
	}
	return items, nil
}

// Resolve discards a parked action or executes it. Confirming runs the
// same executor the chat uses, so a draft confirmed from the dashboard
// writes exactly what answering the bot's question would have written —
// with whatever the user corrected on the way.
func (s *HoldQueueService) Resolve(ctx context.Context, id uuid.UUID, action string, ov *PendingOverrides) ([]*entity.Transaction, error) {
	if action == "discard" {
		ok, err := s.repo.Resolve(ctx, id, action)
		if err != nil {
			return nil, apperror.Internal("failed to resolve pending action").Wrap(err)
		}
		if !ok {
			return nil, apperror.NotFound("pending action not found")
		}
		return nil, nil
	}

	item, err := s.repo.GetOpen(ctx, id)
	if err != nil {
		return nil, apperror.Internal("failed to load pending action").Wrap(err)
	}
	if item == nil {
		return nil, apperror.NotFound("pending action not found")
	}
	if s.executor == nil {
		return nil, apperror.Internal("no executor configured for pending actions")
	}
	return s.executor.Execute(ctx, item, ov)
}
