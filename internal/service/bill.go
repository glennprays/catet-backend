package service

import (
	"context"
	"sort"
	"time"

	"github.com/glennprays/catet-backend/internal/apperror"
	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/params/response"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/google/uuid"
)

// BillService handles the Bill entity feeding Safe-to-Spend
// reservations and the due calendar, and the paid loop that advances a
// bill through its schedule.
type BillService struct {
	repo     *repository.BillRepository
	payments *repository.BillPaymentRepository
	txs      *TransactionService
	sources  *repository.SourceRepository
}

func NewBillService(
	repo *repository.BillRepository,
	payments *repository.BillPaymentRepository,
	txs *TransactionService,
	sources *repository.SourceRepository,
) *BillService {
	return &BillService{repo: repo, payments: payments, txs: txs, sources: sources}
}

func (s *BillService) List(ctx context.Context) ([]entity.Bill, error) {
	bills, err := s.repo.List(ctx)
	if err != nil {
		return nil, apperror.Internal("failed to list bills").Wrap(err)
	}
	return bills, nil
}

// CreateInput is the parsed POST /bills body (dates as YYYY-MM-DD).
type CreateBillInput struct {
	Name            string
	AmountMinMinor  int64
	AmountMaxMinor  int64
	Cadence         string
	NextDueDate     string
	LinkedSourceID  *uuid.UUID
	CategoryID      *uuid.UUID
	RemainingCount  *int
	ReminderOffsets []int
}

// normalizeOffsets de-duplicates and sorts reminder offsets descending
// so the stored value is canonical (3, 1, 0) whatever order the client
// ticked the boxes in. Never nil: an empty array is "no reminders".
func normalizeOffsets(in []int) []int {
	seen := map[int]bool{}
	out := make([]int, 0, len(in))
	for _, v := range in {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(out)))
	return out
}

// tenorAllowed rejects a tenor on a one-time bill: `once` already
// completes after a single payment, so "2 payments left" is a
// contradiction, not a shorthand.
func tenorAllowed(cadence string, remaining *int) error {
	if cadence == "once" && remaining != nil {
		return apperror.Validation("bill validation failed").
			WithDetails(apperror.FieldError{Field: "remaining_count", Rule: "not_allowed_for_once", Message: "a one-time bill has no tenor"})
	}
	return nil
}

func (s *BillService) Create(ctx context.Context, in CreateBillInput) (*entity.Bill, error) {
	due, err := parseJakartaDate(in.NextDueDate)
	if err != nil {
		return nil, apperror.Validation("bill validation failed").
			WithDetails(apperror.FieldError{Field: "next_due_date", Rule: "yyyy-mm-date", Message: "must be YYYY-MM-DD"})
	}
	if in.AmountMaxMinor < in.AmountMinMinor {
		return nil, apperror.Validation("bill validation failed").
			WithDetails(apperror.FieldError{Field: "amount_max_minor", Rule: "gtefield", Message: "band max must be ≥ band min"})
	}
	if err := tenorAllowed(in.Cadence, in.RemainingCount); err != nil {
		return nil, err
	}
	created, err := s.repo.Create(ctx, &entity.Bill{
		ID:             uuid.New(),
		Name:           in.Name,
		AmountMinMinor: in.AmountMinMinor,
		AmountMaxMinor: in.AmountMaxMinor,
		Cadence:        in.Cadence,
		NextDueDate:    due,
		LinkedSourceID: in.LinkedSourceID,
		CategoryID:     in.CategoryID,
		Active:         true,

		RemainingCount:  in.RemainingCount,
		ReminderOffsets: normalizeOffsets(in.ReminderOffsets),
	})
	if err != nil {
		return nil, apperror.Internal("failed to create bill").Wrap(err)
	}
	return created, nil
}

// UpdateInput is the parsed PATCH /bills/{id} body; pointer = present.
type UpdateBillInput struct {
	Name           *string
	AmountMinMinor *int64
	AmountMaxMinor *int64
	Cadence        *string
	NextDueDate    *string
	LinkedSourceID *uuid.UUID
	CategoryID     *uuid.UUID
	// RemainingCount is doubly optional: absent leaves the tenor alone,
	// present-with-nil clears it back to open-ended.
	RemainingCount  *NullableInt
	ReminderOffsets *[]int
}

// NullableInt distinguishes "field absent" from "field set to null" for
// PATCH bodies.
type NullableInt struct {
	Value *int
}

func (s *BillService) Update(ctx context.Context, id uuid.UUID, in UpdateBillInput) (*entity.Bill, error) {
	existing, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, apperror.Internal("failed to load bill").Wrap(err)
	}
	if existing == nil {
		return nil, apperror.NotFound("bill not found")
	}

	fields := map[string]any{}
	if in.Name != nil {
		fields["name"] = *in.Name
	}
	if in.AmountMinMinor != nil {
		fields["amount_min_minor"] = *in.AmountMinMinor
	}
	if in.AmountMaxMinor != nil {
		fields["amount_max_minor"] = *in.AmountMaxMinor
	}
	if in.Cadence != nil {
		fields["cadence"] = *in.Cadence
	}
	if in.NextDueDate != nil && *in.NextDueDate != "" {
		due, err := parseJakartaDate(*in.NextDueDate)
		if err != nil {
			return nil, apperror.Validation("bill validation failed").
				WithDetails(apperror.FieldError{Field: "next_due_date", Rule: "yyyy-mm-date", Message: "must be YYYY-MM-DD"})
		}
		fields["next_due_date"] = due
	}
	if in.LinkedSourceID != nil {
		fields["linked_source_id"] = *in.LinkedSourceID
	}
	if in.CategoryID != nil {
		fields["category_id"] = *in.CategoryID
	}
	if in.RemainingCount != nil {
		if v := in.RemainingCount.Value; v != nil && *v < 1 {
			return nil, apperror.Validation("bill validation failed").
				WithDetails(apperror.FieldError{Field: "remaining_count", Rule: "min", Message: "must be at least 1"})
		}
		fields["remaining_count"] = in.RemainingCount.Value
	}
	if in.ReminderOffsets != nil {
		fields["reminder_offsets"] = normalizeOffsets(*in.ReminderOffsets)
	}

	// Band sanity after the merge of old and new values.
	minor := existing.AmountMinMinor
	maxor := existing.AmountMaxMinor
	if v, ok := fields["amount_min_minor"]; ok {
		minor = v.(int64)
	}
	if v, ok := fields["amount_max_minor"]; ok {
		maxor = v.(int64)
	}
	if maxor < minor {
		return nil, apperror.Validation("bill validation failed").
			WithDetails(apperror.FieldError{Field: "amount_max_minor", Rule: "gtefield", Message: "band max must be ≥ band min"})
	}

	cadence := existing.Cadence
	if in.Cadence != nil {
		cadence = *in.Cadence
	}
	tenor := existing.RemainingCount
	if in.RemainingCount != nil {
		tenor = in.RemainingCount.Value
	}
	if err := tenorAllowed(cadence, tenor); err != nil {
		return nil, err
	}

	updated, err := s.repo.Update(ctx, id, fields)
	if err != nil {
		return nil, apperror.Internal("failed to update bill").Wrap(err)
	}
	return updated, nil
}

// Deactivate soft-disables a bill (kept for history and reporting).
func (s *BillService) Deactivate(ctx context.Context, id uuid.UUID) error {
	existing, err := s.repo.Get(ctx, id)
	if err != nil {
		return apperror.Internal("failed to load bill").Wrap(err)
	}
	if existing == nil {
		return apperror.NotFound("bill not found")
	}
	ok, err := s.repo.SetActive(ctx, id, false)
	if err != nil {
		return apperror.Internal("failed to deactivate bill").Wrap(err)
	}
	if !ok {
		return apperror.Conflict("bill was modified concurrently")
	}
	return nil
}

// Reactivate turns a manually deactivated bill back on. A completed
// bill is not eligible: its schedule is spent, and resurrecting it
// would silently re-reserve money against an obligation that no longer
// exists — create a new bill instead.
func (s *BillService) Reactivate(ctx context.Context, id uuid.UUID) error {
	existing, err := s.repo.Get(ctx, id)
	if err != nil {
		return apperror.Internal("failed to load bill").Wrap(err)
	}
	if existing == nil {
		return apperror.NotFound("bill not found")
	}
	if existing.CompletedAt != nil {
		return apperror.Conflict("bill is completed")
	}
	ok, err := s.repo.SetActive(ctx, id, true)
	if err != nil {
		return apperror.Internal("failed to reactivate bill").Wrap(err)
	}
	if !ok {
		return apperror.Conflict("bill was modified concurrently")
	}
	return nil
}

// PayBillInput settles one occurrence. The amount is always the user's
// number — the band is an estimate the engine never spends on its own.
type PayBillInput struct {
	AmountMinor  int64
	SourceID     *uuid.UUID
	OccurredDate *string
	Origin       string
}

// Pay writes the expense, advances (or completes) the bill, and records
// the link between the two so voiding the transaction can rewind the
// schedule. Ordering matters: the transaction is written first because
// it is the thing that must not be lost; if the advance then loses the
// optimistic race the transaction is voided and the caller retries.
func (s *BillService) Pay(ctx context.Context, id uuid.UUID, in PayBillInput) (*response.BillPaymentData, error) {
	bill, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, apperror.Internal("failed to load bill").Wrap(err)
	}
	if bill == nil {
		return nil, apperror.NotFound("bill not found")
	}
	if !bill.Active {
		return nil, apperror.Conflict("bill is not active")
	}
	if in.AmountMinor <= 0 {
		return nil, apperror.Validation("bill payment validation failed").
			WithDetails(apperror.FieldError{Field: "amount_minor", Rule: "min", Message: "must be at least 1"})
	}

	source := in.SourceID
	if source == nil {
		source = bill.LinkedSourceID
	}
	if source == nil {
		return nil, apperror.Validation("bill payment validation failed").
			WithDetails(apperror.FieldError{Field: "source_id", Rule: "required", Message: "bill has no linked source"})
	}

	date := JakartaDate()
	if in.OccurredDate != nil && *in.OccurredDate != "" {
		date = *in.OccurredDate
	}

	merchant := bill.Name
	// A bill with no category has no picker behind this write: the
	// expense is honestly unlabelled and goes to Needs-Review instead of
	// being refused or guessed at.
	review := ""
	if bill.CategoryID == nil {
		review = "needs_review"
	}
	tx, err := s.txs.Create(ctx, CreateInput{
		Type:         "expense",
		OccurredDate: &date,
		AmountMinor:  in.AmountMinor,
		SourceID:     *source,
		CategoryID:   bill.CategoryID,
		Merchant:     &merchant,
		Origin:       in.Origin,
		ReviewStatus: review,
	})
	if err != nil {
		return nil, err
	}

	next, hasNext := NextOccurrence(bill.Cadence, bill.NextDueDate, bill.AnchorDay)
	completes := !hasNext || (bill.RemainingCount != nil && *bill.RemainingCount <= 1)
	decremented := bill.RemainingCount != nil

	var advancedTo *time.Time
	var moved bool
	if completes {
		moved, err = s.repo.Complete(ctx, id, time.Now().UTC(), decremented)
	} else {
		advancedTo = &next
		moved, err = s.repo.Advance(ctx, id, bill.NextDueDate, next, decremented)
	}
	if err != nil {
		return nil, apperror.Internal("failed to advance bill").Wrap(err)
	}
	if !moved {
		// The bill moved under us; the expense we just wrote is not a
		// settlement of anything, so it does not stay in the ledger.
		if voidErr := s.txs.Void(ctx, tx.ID); voidErr != nil {
			return nil, apperror.Internal("failed to unwind bill payment").Wrap(voidErr)
		}
		return nil, apperror.Conflict("bill was modified concurrently")
	}

	if err := s.payments.Create(ctx, &entity.BillPayment{
		ID:            uuid.New(),
		BillID:        bill.ID,
		TransactionID: tx.ID,
		DueDate:       bill.NextDueDate,
		AdvancedTo:    advancedTo,
		AmountMinor:   in.AmountMinor,
		Decremented:   decremented,
	}); err != nil {
		return nil, apperror.Internal("failed to record bill payment").Wrap(err)
	}

	updated, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, apperror.Internal("failed to reload bill").Wrap(err)
	}
	return &response.BillPaymentData{Bill: updated, Transaction: tx}, nil
}

// Payments lists settled occurrences in [from, to) for the calendar's
// paid markers.
func (s *BillService) Payments(ctx context.Context, from, to string) ([]entity.BillPayment, error) {
	start, err := parseJakartaDate(from)
	if err != nil {
		return nil, err
	}
	end, err := parseJakartaDate(to)
	if err != nil {
		return nil, err
	}
	payments, err := s.payments.ListBetween(ctx, start, end)
	if err != nil {
		return nil, apperror.Internal("failed to list bill payments").Wrap(err)
	}
	return payments, nil
}
