package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/glennprays/catet-backend/internal/apperror"
	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/glennprays/catet-backend/pkg/logctx"
	"github.com/glennprays/log"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// TransactionService owns the ledger engine: leg construction, the
// type/shape rules from ADR-0001, edits with history, voiding and
// reversals. Every write path (bot, dashboard) funnels through here.
type TransactionService struct {
	repo       *repository.TransactionRepository
	sources    *repository.SourceRepository
	categories *repository.CategoryRepository
	// merchants canonicalises the free text every capture surface sends,
	// so "kopi kenangan tp" and "Kopi Kenangan" are one entity.
	merchants *MerchantService
	// Bill links are repository-level deps, not a BillService: the
	// ledger must be able to rewind a bill on void without depending on
	// the service that pays it (BillService already depends on this one).
	billPayments *repository.BillPaymentRepository
	bills        *repository.BillRepository
	log          *log.Logger
}

func NewTransactionService(
	repo *repository.TransactionRepository,
	sources *repository.SourceRepository,
	categories *repository.CategoryRepository,
	merchants *MerchantService,
	billPayments *repository.BillPaymentRepository,
	bills *repository.BillRepository,
	logger *log.Logger,
) *TransactionService {
	return &TransactionService{
		repo:         repo,
		sources:      sources,
		categories:   categories,
		merchants:    merchants,
		billPayments: billPayments,
		bills:        bills,
		log:          logger,
	}
}

// resolveMerchant settles the three ways a capture can name a merchant:
// a pinned id (dashboard picked an existing one), free text (resolved —
// and tidied — through the merchant service), or nothing at all. It
// returns the display name, the entity id and the note to store: when
// the canonical name no longer contains what the user typed, the raw
// phrase survives as the note rather than being lost.
func (s *TransactionService) resolveMerchant(ctx context.Context, in CreateInput) (*uuid.UUID, *string, *string, error) {
	note := in.Note
	if in.MerchantID != nil {
		m, err := s.merchants.Get(ctx, *in.MerchantID)
		if err != nil {
			return nil, nil, nil, err
		}
		name := m.Name
		return &m.ID, &name, note, nil
	}
	if in.Merchant == nil || strings.TrimSpace(*in.Merchant) == "" {
		return nil, in.Merchant, note, nil
	}
	raw := *in.Merchant
	m, err := s.merchants.Resolve(ctx, raw, derefOr(in.Note, ""))
	if err != nil {
		return nil, nil, nil, err
	}
	if m == nil {
		return nil, in.Merchant, note, nil
	}
	if repository.AliasKey(raw) != repository.AliasKey(m.Name) &&
		(note == nil || strings.TrimSpace(*note) == "") {
		phrase := raw
		note = &phrase
	}
	name := m.Name
	return &m.ID, &name, note, nil
}

func derefOr(s *string, alt string) string {
	if s == nil {
		return alt
	}
	return *s
}

// ---------------------------------------------------------------- create

// CreateInput is one validated ledger write. Every capture surface
// (dashboard POST /transactions, WhatsApp bot) funnels through it and
// declares its own provenance — the engine never guesses the origin.
type CreateInput struct {
	Type                string
	OccurredAt          *time.Time
	OccurredDate        *string
	AmountMinor         int64
	SourceID            uuid.UUID
	DestinationSourceID *uuid.UUID
	CategoryID          *uuid.UUID
	Note                *string
	Merchant            *string
	// MerchantID pins the canonical merchant directly (the dashboard's
	// datalist picked one); when set the Merchant text is ignored.
	MerchantID *uuid.UUID

	// Provenance. Origin is required ("dashboard_manual" | "whatsapp_text");
	// CategorySource defaults to "user" when a category is set.
	Origin             string
	CategorySource     *string
	CategoryConfidence *float64
	ReviewStatus       string // "" → "clean"
	ExternalID         *string
	ContentHash        *string
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

// Create writes one capture. Provenance comes from the caller: the
// dashboard declares origin=dashboard_manual / category_source=user
// (ADR-0004 — no AI gate), the bot declares whatsapp_text plus whatever
// its rules or the LLM decided.
func (s *TransactionService) Create(ctx context.Context, in CreateInput) (*entity.Transaction, error) {
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
	switch in.Origin {
	case "dashboard_manual", "whatsapp_text", "whatsapp_photo", "import":
	default:
		fe.add("origin", "oneof", "origin must be dashboard_manual, whatsapp_text, whatsapp_photo or import")
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
		// The dashboard is a deliberate capture surface: the user picked a
		// category there. Bot captures may legitimately arrive unlabelled
		// and are routed to Needs-Review instead of being refused — and so
		// is a dashboard write that has no picker behind it (paying a bill
		// with no category), which says so by asking for needs_review
		// rather than landing an unlabelled row in the clean ledger.
		if in.CategoryID == nil && in.Origin == "dashboard_manual" && in.ReviewStatus != "needs_review" {
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

	merchantID, merchantName, note, err := s.resolveMerchant(ctx, in)
	if err != nil {
		return nil, err
	}

	categorySource := in.CategorySource
	if in.CategoryID != nil && categorySource == nil {
		cs := "user"
		categorySource = &cs
	}
	if in.CategoryID == nil {
		// A category_source without a category would be a dangling claim.
		categorySource = nil
	}
	reviewStatus := in.ReviewStatus
	if reviewStatus == "" {
		reviewStatus = "clean"
	}
	// An uncategorised expense/income can only come from a capture the
	// rules refused to label; it must land in Needs-Review.
	if in.CategoryID == nil && (in.Type == "expense" || in.Type == "income") {
		reviewStatus = "needs_review"
	}
	tx := &entity.Transaction{
		ID:                 uuid.New(),
		OccurredAt:         occurredAt,
		Origin:             in.Origin,
		Type:               in.Type,
		Merchant:           merchantName,
		MerchantID:         merchantID,
		Note:               note,
		AmountMinor:        abs64(in.AmountMinor),
		CategoryID:         in.CategoryID,
		CategorySource:     categorySource,
		CategoryConfidence: in.CategoryConfidence,
		ReviewStatus:       reviewStatus,
		ExternalID:         in.ExternalID,
		ContentHash:        in.ContentHash,
		Legs:               buildLegs(in.Type, in.AmountMinor, &in.SourceID, in.DestinationSourceID, in.CategoryID),
	}
	created, err := s.repo.Insert(ctx, tx)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			// external_id is the bot's write-once key: a duplicate means
			// this capture already landed.
			return nil, apperror.Conflict("duplicate external_id")
		}
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
	// A transaction that settled a bill says so on the detail read, so
	// the dashboard can warn that deleting it rewinds the schedule.
	if p, err := s.billPayments.ByTransaction(ctx, id); err == nil && p != nil {
		name := p.BillName
		tx.BillID = &p.BillID
		tx.BillName = &name
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
	// Voided is "exclude" (default), "include" or "only".
	Voided     *string
	MerchantID *uuid.UUID
	Limit      int
	Cursor     *string
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
		MerchantID: in.MerchantID,
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
	if in.Voided != nil && *in.Voided != "" {
		switch *in.Voided {
		case "exclude", "include", "only":
			f.Voided = *in.Voided
		default:
			return nil, apperror.Validation("invalid filter").
				WithDetails(apperror.FieldError{Field: "voided", Rule: "enum", Message: "voided must be exclude, include or only"})
		}
	}
	// The review queue asks about money that still stands: an original a
	// live reversal has cancelled is settled business, not a question.
	f.ExcludeReversed = in.ReviewStatus != nil && *in.ReviewStatus == "needs_review"

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
	// changes rewrite both of its existing legs. Only the two
	// party-to-party types never carry a category — an expense or income
	// the bot wrote without one is exactly what a correction fixes.
	if in.CategoryID != nil && (tx.Type == "transfer" || tx.Type == "adjustment") {
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
		// Text the user chose on purpose is authoritative as typed; the
		// merchant entity follows it so the row keeps pointing at one
		// canonical name.
		m, err := s.merchants.ResolveUser(ctx, *in.Merchant)
		if err != nil {
			return nil, err
		}
		record("merchant", tx.Merchant, *in.Merchant)
		if m == nil {
			tx.Merchant = editableString(in.Merchant)
			tx.MerchantID = nil
		} else {
			tx.Merchant = &m.Name
			tx.MerchantID = &m.ID
		}
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
	s.revertBillPayment(ctx, id)
	return nil
}

// revertBillPayment rewinds the bill this transaction settled, for both
// ways a payment is undone: a void (dashboard delete) and a reversal
// (`batal` in chat). It is best-effort by design — the undo already
// happened and must not fail because the bill has since been
// rescheduled by hand, which is the newer intent and wins. The link row
// goes either way, so a payment cannot be undone twice.
func (s *TransactionService) revertBillPayment(ctx context.Context, txID uuid.UUID) {
	p, err := s.billPayments.ByTransaction(ctx, txID)
	if err != nil || p == nil {
		return
	}
	reverted, err := s.bills.Revert(ctx, p.BillID, p.AdvancedTo, p.DueDate, p.Decremented)
	if err != nil {
		s.log.Warn(txTraceOf(ctx), "bill revert failed", map[string]any{"bill_id": p.BillID.String(), "error": err.Error()})
	} else if !reverted {
		s.log.Warn(txTraceOf(ctx), "bill payment voided but bill not reverted (edited since)", map[string]any{"bill_id": p.BillID.String()})
	}
	if err := s.billPayments.Delete(ctx, p.ID); err != nil {
		s.log.Warn(txTraceOf(ctx), "bill payment link not deleted", map[string]any{"error": err.Error()})
	}
}

// -------------------------------------------------------------- reversal

// Reverse creates a linked reversal with mirrored (negated) legs. Both
// transactions stay visible; nothing collapses into a silent edit. The
// reversal carries the origin of the surface that asked for it.
func (s *TransactionService) Reverse(ctx context.Context, id uuid.UUID, note *string, origin string) (*entity.Transaction, error) {
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

	// The reversal mirrors the original's category attribution verbatim —
	// claiming "user" would invent provenance the user never gave.
	categorySource := tx.CategorySource
	reversal := &entity.Transaction{
		ID:                    uuid.New(),
		OccurredAt:            time.Now().UTC(),
		Origin:                origin,
		Type:                  tx.Type,
		Merchant:              tx.Merchant,
		MerchantID:            tx.MerchantID,
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
	// A reversed bill payment is an unpaid occurrence again. Both rows
	// stay visible (Principle 5) — it is the schedule that rewinds.
	s.revertBillPayment(ctx, id)
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

// txTraceOf keeps log calls correlated; the logger refuses an empty id.
func txTraceOf(ctx context.Context) string {
	if id := logctx.TraceID(ctx); id != "" {
		return id
	}
	return "ledger"
}
