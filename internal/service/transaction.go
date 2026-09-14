package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/glennprays/catet-backend/internal/apperror"
	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/google/uuid"
)

// TransactionService owns the ledger engine: leg construction, the
// type/shape rules from ADR-0001, edits with history, voiding and
// reversals. Every write path (bot, dashboard) funnels through here.
type TransactionService struct {
	repo       *repository.TransactionRepository
	sources    *repository.SourceRepository
	categories *repository.CategoryRepository
}

func NewTransactionService(
	repo *repository.TransactionRepository,
	sources *repository.SourceRepository,
	categories *repository.CategoryRepository,
) *TransactionService {
	return &TransactionService{repo: repo, sources: sources, categories: categories}
}

// ---------------------------------------------------------------- create

// CreateManualInput is the validated payload of POST /transactions.
type CreateManualInput struct {
	Type                string
	OccurredAt          *time.Time
	OccurredDate        *string
	AmountMinor         int64
	SourceID            uuid.UUID
	DestinationSourceID *uuid.UUID
	CategoryID          *uuid.UUID
	Note                *string
	Merchant            *string
}

// fieldErrors accumulates validation failures; non-empty at the end of
// validation turns into one VALIDATION_FAILED error.
type fieldErrors struct {
	errs []apperror.FieldError
}

func (f *fieldErrors) add(field, rule, message string) {
	f.errs = append(f.errs, apperror.FieldError{Field: field, Rule: rule, Message: message})
}

func (f *fieldErrors) toError() error {
	if len(f.errs) == 0 {
		return nil
	}
	return apperror.Validation("transaction validation failed").WithDetails(f.errs...)
}

// CreateManual writes a dashboard peer capture: origin=dashboard_manual,
// category_source=user, review_status=clean (ADR-0004 — no AI gate).
func (s *TransactionService) CreateManual(ctx context.Context, in CreateManualInput) (*entity.Transaction, error) {
	fe := &fieldErrors{}

	// Both absent means "now" — the capture happened just now.
	occurredAt := time.Now().UTC()
	if in.OccurredAt != nil {
		occurredAt = in.OccurredAt.UTC()
	} else if in.OccurredDate != nil && *in.OccurredDate != "" {
		t, err := JakartaNoon(*in.OccurredDate)
		if err != nil {
			fe.add("occurred_date", "format", "must be YYYY-MM-DD")
		} else {
			occurredAt = t
		}
	}

	if in.AmountMinor == 0 {
		fe.add("amount_minor", "nonzero", "amount must not be zero")
	}
	if in.Type != "adjustment" && in.AmountMinor < 0 {
		fe.add("amount_minor", "positive", "amount must be positive")
	}

	// Referenced rows must exist — body references fail closed as
	// validation errors, not 404 (the path resource is /transactions).
	if in.SourceID != uuid.Nil {
		src, err := s.sources.Get(ctx, in.SourceID)
		if err != nil {
			return nil, apperror.Internal("failed to load source").Wrap(err)
		}
		if src == nil {
			fe.add("source_id", "exists", "source not found")
		}
	} else {
		fe.add("source_id", "required", "source is required")
	}
	if in.DestinationSourceID != nil {
		dst, err := s.sources.Get(ctx, *in.DestinationSourceID)
		if err != nil {
			return nil, apperror.Internal("failed to load destination source").Wrap(err)
		}
		if dst == nil {
			fe.add("destination_source_id", "exists", "destination source not found")
		}
	}
	if in.CategoryID != nil {
		cat, err := s.categories.Get(ctx, *in.CategoryID)
		if err != nil {
			return nil, apperror.Internal("failed to load category").Wrap(err)
		}
		if cat == nil {
			fe.add("category_id", "exists", "category not found")
		}
	}

	switch in.Type {
	case "expense", "income":
		if in.CategoryID == nil {
			fe.add("category_id", "required", "category is required")
		}
		if in.DestinationSourceID != nil {
			fe.add("destination_source_id", "forbidden", "destination only applies to transfers")
		}
	case "transfer":
		if in.DestinationSourceID == nil {
			fe.add("destination_source_id", "required", "transfer destination is required")
		} else if *in.DestinationSourceID == in.SourceID {
			fe.add("destination_source_id", "distinct", "destination must differ from source")
		}
		if in.CategoryID != nil {
			fe.add("category_id", "forbidden", "transfers never carry a category")
		}
	case "adjustment":
		if in.CategoryID != nil {
			fe.add("category_id", "forbidden", "adjustments never carry a category")
		}
		if in.DestinationSourceID != nil {
			fe.add("destination_source_id", "forbidden", "destination only applies to transfers")
		}
	default:
		fe.add("type", "oneof", "type must be expense, income, transfer or adjustment")
	}
	if err := fe.toError(); err != nil {
		return nil, err
	}

	var categorySource *string
	if in.CategoryID != nil {
		cs := "user"
		categorySource = &cs
	}
	tx := &entity.Transaction{
		ID:             uuid.New(),
		OccurredAt:     occurredAt,
		Origin:         "dashboard_manual",
		Type:           in.Type,
		Merchant:       in.Merchant,
		Note:           in.Note,
		AmountMinor:    abs64(in.AmountMinor),
		CategoryID:     in.CategoryID,
		CategorySource: categorySource,
		ReviewStatus:   "clean",
		Legs:           buildLegs(in.Type, in.AmountMinor, &in.SourceID, in.DestinationSourceID, in.CategoryID),
	}
	created, err := s.repo.Insert(ctx, tx)
	if err != nil {
		return nil, apperror.Internal("failed to write transaction").Wrap(err)
	}
	return created, nil
}

// Adjust writes an adjustment journal correcting a source's ledger
// balance to observed reality (flows.md §8). Signed amount: positive
// tops the source up, negative marks it down.
func (s *TransactionService) Adjust(ctx context.Context, sourceID uuid.UUID, amountMinor int64, note *string) (*entity.Transaction, error) {
	src, err := s.sources.Get(ctx, sourceID)
	if err != nil {
		return nil, apperror.Internal("failed to load source").Wrap(err)
	}
	if src == nil {
		return nil, apperror.NotFound("source not found")
	}
	if amountMinor == 0 {
		return nil, apperror.Validation("adjustment validation failed").
			WithDetails(apperror.FieldError{Field: "amount_minor", Rule: "nonzero", Message: "adjustment must not be zero"})
	}

	tx := &entity.Transaction{
		ID:           uuid.New(),
		OccurredAt:   time.Now().UTC(),
		Origin:       "dashboard_manual",
		Type:         "adjustment",
		Note:         note,
		AmountMinor:  abs64(amountMinor),
		ReviewStatus: "clean",
		Legs:         buildLegs("adjustment", amountMinor, &sourceID, nil, nil),
	}
	created, err := s.repo.Insert(ctx, tx)
	if err != nil {
		return nil, apperror.Internal("failed to write adjustment").Wrap(err)
	}
	return created, nil
}

// ------------------------------------------------------------------ read

// Get returns one transaction with legs and edit history.
func (s *TransactionService) Get(ctx context.Context, id uuid.UUID) (*entity.Transaction, error) {
	tx, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, apperror.Internal("failed to load transaction").Wrap(err)
	}
	if tx == nil {
		return nil, apperror.NotFound("transaction not found")
	}
	return tx, nil
}

// ListInput carries the parsed GET /transactions query.
type ListInput struct {
	SourceID     *uuid.UUID
	CategoryID   *uuid.UUID
	ReviewStatus *string
	Query        *string
	FromDate     *string
	ToDate       *string
	Direction    *string
	Limit        int
	Cursor       *string
}

// ListPage is one keyset page plus its continuation cursor.
type ListPage struct {
	Rows       []repository.LedgerRow
	Total      int64
	NextCursor *string
}

// List resolves filters, decodes the cursor and returns a page with
// the matching total count.
func (s *TransactionService) List(ctx context.Context, in ListInput) (*ListPage, error) {
	f := repository.ListFilter{
		SourceID:   in.SourceID,
		CategoryID: in.CategoryID,
	}

	if in.FromDate != nil && *in.FromDate != "" {
		from, _, err := JakartaDayBounds(*in.FromDate)
		if err != nil {
			return nil, err
		}
		f.From = &from
	}
	if in.ToDate != nil && *in.ToDate != "" {
		_, to, err := JakartaDayBounds(*in.ToDate)
		if err != nil {
			return nil, err
		}
		f.To = &to
	}
	if in.Direction != nil && *in.Direction != "" {
		switch *in.Direction {
		case "expense", "income", "transfer", "adjustment":
			f.Direction = in.Direction
		default:
			return nil, apperror.Validation("invalid direction").
				WithDetails(apperror.FieldError{Field: "direction", Rule: "oneof", Message: "direction must be expense, income, transfer or adjustment"})
		}
	}
	if in.ReviewStatus != nil && *in.ReviewStatus != "" {
		f.ReviewStatus = in.ReviewStatus
	}
	if in.Query != nil && *in.Query != "" {
		f.Query = in.Query
	}

	f.Limit = in.Limit
	if f.Limit <= 0 {
		f.Limit = 50
	}
	if f.Limit > 200 {
		f.Limit = 200
	}

	if in.Cursor != nil && *in.Cursor != "" {
		cur, err := decodeCursor(*in.Cursor)
		if err != nil {
			return nil, apperror.BadRequest("invalid cursor")
		}
		f.Cursor = cur
	}

	rows, err := s.repo.List(ctx, f)
	if err != nil {
		return nil, apperror.Internal("failed to list transactions").Wrap(err)
	}
	total, err := s.repo.Count(ctx, f)
	if err != nil {
		return nil, apperror.Internal("failed to count transactions").Wrap(err)
	}

	page := &ListPage{Rows: rows, Total: total}
	if len(rows) == f.Limit {
		last := rows[len(rows)-1]
		cur := encodeCursor(last.OccurredAt, last.TransactionID)
		page.NextCursor = &cur
	}
	return page, nil
}

// ---------------------------------------------------------------- update

// UpdateInput is the parsed PATCH body: pointer = field present.
type UpdateInput struct {
	AmountMinor *int64
	OccurredAt  *time.Time
	CategoryID  *uuid.UUID
	Note        *string
	Merchant    *string
}

// Update rewrites the leg set and appends one edit_history entry.
func (s *TransactionService) Update(ctx context.Context, id uuid.UUID, in UpdateInput) (*entity.Transaction, error) {
	tx, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, apperror.Internal("failed to load transaction").Wrap(err)
	}
	if tx == nil {
		return nil, apperror.NotFound("transaction not found")
	}
	if tx.VoidedAt != nil {
		return nil, apperror.Conflict("voided transactions cannot be edited")
	}

	fe := &fieldErrors{}
	if in.AmountMinor != nil && *in.AmountMinor == 0 {
		fe.add("amount_minor", "nonzero", "amount must not be zero")
	}
	// The destination source of a transfer is never editable; amount
	// changes rewrite both of its existing legs.
	if in.CategoryID != nil && tx.CategoryID == nil {
		fe.add("category_id", "forbidden", "this transaction type never carries a category")
	}
	if err := fe.toError(); err != nil {
		return nil, err
	}
	if in.CategoryID != nil {
		cat, err := s.categories.Get(ctx, *in.CategoryID)
		if err != nil {
			return nil, apperror.Internal("failed to load category").Wrap(err)
		}
		if cat == nil {
			return nil, apperror.Validation("transaction validation failed").
				WithDetails(apperror.FieldError{Field: "category_id", Rule: "exists", Message: "category not found"})
		}
	}

	changes := map[string]map[string]any{}
	record := func(field string, from, to any) {
		changes[field] = map[string]any{"from": from, "to": to}
	}

	if in.AmountMinor != nil {
		record("amount_minor", tx.AmountMinor, *in.AmountMinor)
		tx.AmountMinor = abs64(*in.AmountMinor)
	}
	if in.OccurredAt != nil {
		record("occurred_at", tx.OccurredAt, in.OccurredAt.UTC())
		tx.OccurredAt = in.OccurredAt.UTC()
	}
	if in.Note != nil {
		record("note", tx.Note, *in.Note)
		tx.Note = editableString(in.Note)
	}
	if in.Merchant != nil {
		record("merchant", tx.Merchant, *in.Merchant)
		tx.Merchant = editableString(in.Merchant)
	}
	if in.CategoryID != nil {
		record("category_id", tx.CategoryID, *in.CategoryID)
		tx.CategoryID = in.CategoryID
		// A user-corrected category is authoritative (ADR-0003
		// provenance) and resolves the Needs-Review queue entry.
		user := "user"
		record("category_source", tx.CategorySource, user)
		tx.CategorySource = &user
		if tx.ReviewStatus != "clean" {
			record("review_status", tx.ReviewStatus, "clean")
			tx.ReviewStatus = "clean"
		}
	}

	if len(changes) == 0 {
		return tx, nil
	}

	// Rebuild legs from the existing party identity, swapping in the
	// new amount/category.
	parties := legsParties(tx)
	switch tx.Type {
	case "transfer":
		tx.Legs = buildLegs("transfer", tx.AmountMinor, parties.Source, parties.Dest, nil)
	case "adjustment":
		tx.Legs = buildLegs("adjustment", parties.SourceSign*tx.AmountMinor, parties.Source, nil, nil)
	case "income":
		tx.Legs = buildLegs("income", tx.AmountMinor, parties.Source, nil, tx.CategoryID)
	default: // expense
		tx.Legs = buildLegs("expense", tx.AmountMinor, parties.Source, nil, tx.CategoryID)
	}

	if err := s.appendEditHistory(ctx, tx, changes); err != nil {
		return nil, err
	}
	saved, err := s.repo.Save(ctx, tx)
	if err != nil {
		return nil, apperror.Internal("failed to save transaction").Wrap(err)
	}
	return saved, nil
}

// appendEditHistory marshals one {edited_at, changes} entry onto the
// transaction's history array.
func (s *TransactionService) appendEditHistory(ctx context.Context, tx *entity.Transaction, changes map[string]map[string]any) error {
	history := []map[string]any{}
	if len(tx.EditHistory) > 0 {
		if err := json.Unmarshal(tx.EditHistory, &history); err != nil {
			return apperror.Internal("failed to parse edit history").Wrap(err)
		}
	}
	history = append(history, map[string]any{
		"edited_at": time.Now().UTC().Format(time.RFC3339Nano),
		"changes":   changes,
	})
	updated, err := json.Marshal(history)
	if err != nil {
		return apperror.Internal("failed to encode edit history").Wrap(err)
	}
	tx.EditHistory = updated
	return nil
}

// ------------------------------------------------------------------ void

// Void soft-deletes: excluded from every computation, still listed.
func (s *TransactionService) Void(ctx context.Context, id uuid.UUID) error {
	tx, err := s.repo.Get(ctx, id)
	if err != nil {
		return apperror.Internal("failed to load transaction").Wrap(err)
	}
	if tx == nil {
		return apperror.NotFound("transaction not found")
	}
	if tx.VoidedAt != nil {
		return nil // idempotent
	}
	ok, err := s.repo.SetVoided(ctx, id, time.Now().UTC())
	if err != nil {
		return apperror.Internal("failed to void transaction").Wrap(err)
	}
	if !ok {
		return apperror.Conflict("transaction was modified concurrently")
	}
	return nil
}

// -------------------------------------------------------------- reversal

// Reverse creates a linked reversal with mirrored (negated) legs. Both
// transactions stay visible; nothing collapses into a silent edit.
func (s *TransactionService) Reverse(ctx context.Context, id uuid.UUID, note *string) (*entity.Transaction, error) {
	tx, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, apperror.Internal("failed to load transaction").Wrap(err)
	}
	if tx == nil {
		return nil, apperror.NotFound("transaction not found")
	}
	if tx.VoidedAt != nil {
		return nil, apperror.Conflict("voided transactions cannot be reversed")
	}
	if tx.ReversesTransactionID != nil {
		return nil, apperror.Conflict("this transaction is itself a reversal")
	}

	legs := make([]entity.Leg, len(tx.Legs))
	for i, l := range tx.Legs {
		legs[i] = entity.Leg{
			ID:           uuid.New(),
			SourceID:     l.SourceID,
			CategoryID:   l.CategoryID,
			SignedAmount: -l.SignedAmount,
		}
	}

	var categorySource *string
	if tx.CategoryID != nil {
		cs := "user"
		categorySource = &cs
	}
	reversal := &entity.Transaction{
		ID:                    uuid.New(),
		OccurredAt:            time.Now().UTC(),
		Origin:                "dashboard_manual",
		Type:                  tx.Type,
		Merchant:              tx.Merchant,
		Note:                  note,
		AmountMinor:           tx.AmountMinor,
		CategoryID:            tx.CategoryID,
		CategorySource:        categorySource,
		ReviewStatus:          "clean",
		ReversesTransactionID: &tx.ID,
		Legs:                  legs,
	}
	created, err := s.repo.Insert(ctx, reversal)
	if err != nil {
		return nil, apperror.Internal("failed to write reversal").Wrap(err)
	}
	return created, nil
}

// ---------------------------------------------------------------- helpers

// buildLegs constructs the two-leg journal per ADR-0001:
//
//	expense    source −a, category +a
//	income     source +a, category −a
//	transfer   source −a, destination +a
//	adjustment source  a, system sink −a (both NULL)
func buildLegs(txType string, amount int64, sourceID, destID, categoryID *uuid.UUID) []entity.Leg {
	switch txType {
	case "expense":
		return []entity.Leg{
			{ID: uuid.New(), SourceID: sourceID, SignedAmount: -amount},
			{ID: uuid.New(), CategoryID: categoryID, SignedAmount: amount},
		}
	case "income":
		return []entity.Leg{
			{ID: uuid.New(), SourceID: sourceID, SignedAmount: amount},
			{ID: uuid.New(), CategoryID: categoryID, SignedAmount: -amount},
		}
	case "transfer":
		return []entity.Leg{
			{ID: uuid.New(), SourceID: sourceID, SignedAmount: -amount},
			{ID: uuid.New(), SourceID: destID, SignedAmount: amount},
		}
	case "adjustment":
		return []entity.Leg{
			{ID: uuid.New(), SourceID: sourceID, SignedAmount: amount},
			{ID: uuid.New(), SignedAmount: -amount},
		}
	default:
		panic(fmt.Sprintf("buildLegs: unknown transaction type %q", txType))
	}
}

// legParties describes where money came from / went in an existing leg
// set, so edits can rebuild legs without touching party identity.
type legParties struct {
	Source     *uuid.UUID // outbound/primary source leg
	Dest       *uuid.UUID // transfer destination
	SourceSign int64      // sign of the primary source leg (adjustment direction)
}

// legsParties extracts the party identity from an existing leg set.
func legsParties(tx *entity.Transaction) legParties {
	var out legParties
	for i := range tx.Legs {
		l := &tx.Legs[i]
		if l.SourceID == nil {
			continue
		}
		id := *l.SourceID
		switch {
		case l.SignedAmount < 0:
			out.Source = &id
		case tx.Type == "transfer":
			out.Dest = &id
		default:
			out.Source = &id
			out.SourceSign = 1
		}
	}
	if out.SourceSign == 0 {
		out.SourceSign = -1
	}
	return out
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// editableString converts an editable string field: empty string clears
// (nil), anything else sets.
func editableString(s *string) *string {
	if s == nil || *s == "" {
		return nil
	}
	return s
}

// Cursor encoding: base64url of "v1|<unixnano>|<uuid>" — opaque to
// clients, stable across requests.
func encodeCursor(at time.Time, id uuid.UUID) string {
	return encodeCursorValue(fmt.Sprintf("v1|%d|%s", at.UnixNano(), id))
}

