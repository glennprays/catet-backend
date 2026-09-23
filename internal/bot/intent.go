package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/glennprays/catet-backend/internal/apperror"
	"github.com/glennprays/catet-backend/internal/bot/parse"
	"github.com/glennprays/catet-backend/internal/bot/tmpl"
	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/llm"
	"github.com/glennprays/catet-backend/internal/money"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/glennprays/catet-backend/internal/service"
	waga "github.com/glennprays/whatsapp-gateway-sdk-go"
	"github.com/google/uuid"
)

// intentStage handles every message the deterministic rules refused to
// turn into a write. The router only classifies and picks ids from the
// whitelists; money is still verified here against the message text, so
// a model that invents an amount changes nothing (ADR-0003).
func (h *Harness) intentStage(
	ctx context.Context,
	p *waga.IncomingWebhookPayload,
	parsed parse.Parsed,
	state *repository.ChatState,
	lang string,
) (string, []uuid.UUID, string, error) {
	if !h.llm.Enabled(ctx) {
		return h.intentFallback(ctx, p, parsed, state, lang)
	}

	req, err := h.intentRequest(ctx, p, state, lang)
	if err != nil {
		return "", nil, lang, err
	}
	in, err := h.llm.Intent(ctx, *req)
	if err != nil {
		if !errors.Is(err, llm.ErrUnavailable) {
			h.log.Warn(traceOf(ctx), "intent router failed", map[string]any{"error": err.Error()})
		}
		return h.intentFallback(ctx, p, parsed, state, lang)
	}

	reply, ids, err := h.runIntent(ctx, p, in, state, lang, parsed)
	return reply, ids, lang, err
}

// intentFallback is the no-LLM behaviour: the rules alone. A message
// carrying money still captures (holds included); anything else gets one
// honest "not understood" instead of a junk Hold Queue row.
func (h *Harness) intentFallback(
	ctx context.Context,
	p *waga.IncomingWebhookPayload,
	parsed parse.Parsed,
	state *repository.ChatState,
	lang string,
) (string, []uuid.UUID, string, error) {
	if len(parsed.Drafts) > 0 && parse.HasMoneyToken(p.Text) {
		// A new capture supersedes the pending question: leaving it open
		// would make the next bare Source name answer a draft the user has
		// already moved past. The Hold Queue row stays for the dashboard.
		if state.OpenPendingActionID != nil {
			if err := h.clearQuestion(ctx, state); err != nil {
				return "", nil, lang, err
			}
		}
		return h.capture(ctx, p, wrapDrafts(parsed.Drafts), state, lang, parsed.SourceFooter)
	}
	// Without a model there is nothing left to try, but the question is
	// still the user's open business: repeat it instead of dropping it.
	reply, _, err := h.remind(ctx, state, lang, "")
	return reply, nil, lang, err
}

// intentRequest assembles everything the router may see: the open
// question, the last chat batch and the two whitelists.
func (h *Harness) intentRequest(
	ctx context.Context,
	p *waga.IncomingWebhookPayload,
	state *repository.ChatState,
	lang string,
) (*llm.IntentRequest, error) {
	sources, err := h.sources.List(ctx, false)
	if err != nil {
		return nil, fmt.Errorf("list sources: %w", err)
	}
	cats, err := h.categories.List(ctx, false)
	if err != nil {
		return nil, fmt.Errorf("list categories: %w", err)
	}

	req := &llm.IntentRequest{
		Lang:       lang,
		Text:       p.Text,
		Today:      service.JakartaDate(),
		Sources:    make([]llm.Candidate, 0, len(sources)),
		Categories: make([]llm.Candidate, 0, len(cats)),
	}
	for _, s := range sources {
		hint := s.Type
		if s.IsDebt {
			hint += ", debt"
		}
		req.Sources = append(req.Sources, llm.Candidate{ID: s.ID, Name: s.Name, Hint: hint})
	}
	for _, c := range cats {
		req.Categories = append(req.Categories, llm.Candidate{ID: c.ID, Name: c.Name, Hint: parentName(c, cats)})
	}

	if state.OpenPendingActionID != nil {
		pending, err := h.pending.GetOpen(ctx, *state.OpenPendingActionID)
		if err != nil {
			return nil, fmt.Errorf("load pending action: %w", err)
		}
		if pending != nil {
			expect := ""
			if state.OpenExpect != nil {
				expect = *state.OpenExpect
			}
			req.Open = &llm.OpenQuestion{Expect: expect, Summary: pending.Summary}
		}
	}

	recent, err := h.recentBatch(ctx, state, sources)
	if err != nil {
		return nil, err
	}
	for i, tx := range recent {
		req.Recent = append(req.Recent, llm.RecentItem{
			N: i + 1, Type: tx.Type, Merchant: valueOr(tx.Merchant, ""),
			Amount: money.FormatIDR(tx.AmountMinor), Category: valueOr(tx.CategoryName, ""),
			Source: transactionSourceName(&recent[i]),
		})
	}
	return req, nil
}

// recentBatch loads the transactions the chat's last batch wrote, oldest
// first, skipping anything already voided or reversed: a correction can
// only address a line that is still standing.
func (h *Harness) recentBatch(
	ctx context.Context,
	state *repository.ChatState,
	_ []entity.Source,
) ([]entity.Transaction, error) {
	if state.LastBatchMessageID == nil {
		return nil, nil
	}
	batch, err := h.inbound.Get(ctx, *state.LastBatchMessageID)
	if err != nil {
		return nil, fmt.Errorf("load batch: %w", err)
	}
	if batch == nil {
		return nil, nil
	}
	out := make([]entity.Transaction, 0, len(batch.TransactionIDs))
	for _, id := range batch.TransactionIDs {
		tx, err := h.txRepo.Get(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("load transaction: %w", err)
		}
		if tx == nil || tx.VoidedAt != nil {
			continue
		}
		reversed, err := h.txRepo.IsReversed(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("check reversal: %w", err)
		}
		if reversed {
			continue
		}
		out = append(out, *tx)
	}
	return out, nil
}

// runIntent executes one classified message.
func (h *Harness) runIntent(
	ctx context.Context,
	p *waga.IncomingWebhookPayload,
	in *llm.Intent,
	state *repository.ChatState,
	lang string,
	parsed parse.Parsed,
) (string, []uuid.UUID, error) {
	// The open question outlives almost everything: it is the only state
	// a message can silently break, and a stranded draft is how a receipt
	// becomes dashboard-only work. It is closed here in exactly two
	// cases — the message answered it, or the message threw it away.
	reminder := ""
	if state.OpenPendingActionID != nil {
		switch {
		case in.Kind == llm.KindAnswer && in.Answer != nil:
			edits := &answerEdits{AnswerFields: *in.Answer}
			// The rules read the message too: a date they can see beats
			// whatever the router claimed about it.
			if _, when, found := parse.ExtractTime(p.Text, service.JakartaNow()); found {
				edits.OccurredAt = &when
			}
			reply, ids, handled, err := h.applyAnswer(ctx, edits, p.MessageId, state, lang)
			if err != nil {
				return "", nil, err
			}
			if handled {
				return reply, ids, nil
			}
			// The router called it an answer and the rules could not use
			// it. Asking again is better than dropping the draft.
			return h.remind(ctx, state, lang, "")

		case in.Kind == llm.KindUndo:
			pendingID := *state.OpenPendingActionID
			if _, err := h.pending.Resolve(ctx, pendingID, "discard"); err != nil {
				return "", nil, fmt.Errorf("discard pending action: %w", err)
			}
			if err := h.clearQuestion(ctx, state); err != nil {
				return "", nil, err
			}
			return tmpl.Render(lang, "discarded", nil), nil, nil

		case in.Kind == llm.KindCapture && len(in.Drafts) > 0:
			// A new capture supersedes the question: leaving it open would
			// make the next bare Source name answer a draft the user has
			// already moved past. The Hold Queue row stays for the
			// dashboard.
			if err := h.clearQuestion(ctx, state); err != nil {
				return "", nil, err
			}

		default:
			// Anything else is answered on its own terms, and the question
			// is repeated after it.
			reminder = h.openQuestionReminder(ctx, state, lang)
		}
	}

	switch in.Kind {
	case llm.KindCapture:
		if len(in.Drafts) == 0 {
			return h.remind(ctx, state, lang, "")
		}
		reply, ids, _, err := h.capture(ctx, p, h.intentDrafts(in.Drafts, p.Text, lang, parsed.Drafts), state, lang, parsed.SourceFooter)
		return reply, ids, err

	case llm.KindCorrect:
		reply, ids, err := h.correct(ctx, p, in.Correction, state, lang)
		return withReminder(reply, reminder), ids, err

	case llm.KindUndo:
		reply, ids, err := h.batal(ctx, state, in.UndoIndex, lang)
		return withReminder(reply, reminder), ids, err

	case llm.KindSummary:
		switch in.Period {
		case "today":
			reply, err := h.rekapHari(ctx, lang)
			return withReminder(reply, reminder), nil, err
		case "week":
			reply, err := h.rekapMinggu(ctx, lang)
			return withReminder(reply, reminder), nil, err
		}
		return h.remind(ctx, state, lang, reminder)

	case llm.KindBalance:
		reply, err := h.balance(ctx, in.BalanceSource, lang)
		return withReminder(reply, reminder), nil, err

	default:
		return h.remind(ctx, state, lang, reminder)
	}
}

// remind answers a message the bot could not use. With a question still
// open it repeats that question instead of the generic "not understood"
// — the draft is what the user is actually waiting on.
func (h *Harness) remind(
	ctx context.Context, state *repository.ChatState, lang, reminder string,
) (string, []uuid.UUID, error) {
	if reminder == "" {
		reminder = h.openQuestionReminder(ctx, state, lang)
	}
	if reminder == "" {
		return tmpl.Render(lang, "unknown", nil), nil, nil
	}
	return reminder, nil, nil
}

// openQuestionReminder re-states the outstanding question. A draft that
// was resolved elsewhere leaves nothing to remind about, and the chat's
// stale pointer is cleared on the way out.
func (h *Harness) openQuestionReminder(ctx context.Context, state *repository.ChatState, lang string) string {
	if state.OpenPendingActionID == nil {
		return ""
	}
	pending, err := h.pending.GetOpen(ctx, *state.OpenPendingActionID)
	if err != nil {
		h.log.Warn(traceOf(ctx), "load pending action failed", map[string]any{"error": err.Error()})
		return ""
	}
	if pending == nil {
		if err := h.clearQuestion(ctx, state); err != nil {
			h.log.Warn(traceOf(ctx), "clear stale question failed", map[string]any{"error": err.Error()})
		}
		return ""
	}
	return tmpl.Render(lang, "open_question_reminder", map[string]any{"Summary": pending.Summary})
}

// withReminder appends the outstanding question to a reply that answered
// something else.
func withReminder(reply, reminder string) string {
	if reminder == "" || reply == "" {
		return reply
	}
	return reply + "\n" + reminder
}

// intentDrafts turns the router's reading into write-path drafts. Every
// amount is re-derived from the message: the model only points at the
// token, the money grammar decides what it is worth, and a token that is
// not in the message is no amount at all.
func (h *Harness) intentDrafts(in []llm.IntentDraft, text, lang string, parserDrafts []parse.Draft) []draft {
	out := make([]draft, 0, len(in))
	for i, d := range in {
		// The deterministic grammar saw the actual words, so it outranks
		// the router's reading; capture() judges whatever comes out.
		occurred := service.JakartaNow()
		if _, when, found := parse.ExtractTime(text, occurred); found {
			occurred = when
		} else if d.OccurredDate != "" {
			if parsed, err := service.JakartaNoon(d.OccurredDate); err == nil {
				occurred = parsed
			}
		}
		built := draft{Draft: parse.Draft{
			Type:       d.Type,
			Merchant:   strings.ToLower(strings.TrimSpace(d.Merchant)),
			OccurredAt: occurred,
			Lang:       lang,
		}}
		if amount, ok := verifyAmount(text, d.AmountText); ok {
			built.AmountMinor = amount
		} else {
			built.Hold = &parse.HoldReason{Code: parse.HoldNoAmount, Question: "q_amount"}
		}
		if i < len(parserDrafts) {
			// Keep parser-owned Source clauses on the corresponding
			// line. Model ids remain an augmenting hint for lines with
			// no deterministic Source clause.
			built.Pays = append([]string(nil), parserDrafts[i].Pays...)
			built.Froms = append([]string(nil), parserDrafts[i].Froms...)
			built.Places = append([]string(nil), parserDrafts[i].Places...)
			built.SourceFromParser = true
		}
		built.SourceID = d.SourceID
		built.DestinationID = d.DestinationSourceID
		built.CategoryID = d.CategoryID
		out = append(out, built)
	}
	return out
}

// correct edits one transaction of the last chat batch. A category,
// merchant, amount or date is a PATCH in place; a different Source is a
// reversal plus a re-write, because a leg's party is its identity
// (ADR-0001).
func (h *Harness) correct(
	ctx context.Context,
	p *waga.IncomingWebhookPayload,
	corr *llm.Correction,
	state *repository.ChatState,
	lang string,
) (string, []uuid.UUID, error) {
	if corr == nil {
		return tmpl.Render(lang, "unknown", nil), nil, nil
	}
	sources, err := h.sources.List(ctx, false)
	if err != nil {
		return "", nil, fmt.Errorf("list sources: %w", err)
	}
	recent, err := h.recentBatch(ctx, state, sources)
	if err != nil {
		return "", nil, err
	}
	if len(recent) == 0 {
		return tmpl.Render(lang, "correct_nothing", nil), nil, nil
	}
	idx := corr.TargetIndex - 1
	if corr.TargetIndex == 0 || idx < 0 || idx >= len(recent) {
		idx = len(recent) - 1
	}
	target := recent[idx]

	// The date is settled before anything is written: a refused date must
	// not leave a half-applied correction behind.
	var correctedAt *time.Time
	if _, when, found := parse.ExtractTime(p.Text, service.JakartaNow()); found {
		guarded, refusal := guardDate(when, lang)
		if refusal != "" {
			return refusal, nil, nil
		}
		correctedAt = guarded
	} else if corr.OccurredDate != nil {
		if parsed, err := service.JakartaNoon(*corr.OccurredDate); err == nil {
			guarded, refusal := guardDate(parsed, lang)
			if refusal != "" {
				return refusal, nil, nil
			}
			correctedAt = guarded
		}
	}

	var writtenIDs []uuid.UUID
	if corr.SourceID != nil && *corr.SourceID != currentSourceID(&target) {
		replaced, err := h.rewriteSource(ctx, p, &target, *corr.SourceID, state)
		if err != nil {
			var fieldErr *apperror.Error
			if errors.As(err, &fieldErr) && fieldErr.Kind == apperror.KindValidation {
				return tmpl.Render(lang, "correct_failed", map[string]any{
					"Field": fieldLabel(fieldErr, lang),
				}), nil, nil
			}
			return "", nil, err
		}
		target = *replaced
		writtenIDs = append(writtenIDs, replaced.ID)
	}

	update := service.UpdateInput{CategoryID: corr.CategoryID}
	if corr.Merchant != nil {
		merchant := strings.ToLower(strings.TrimSpace(*corr.Merchant))
		update.Merchant = &merchant
	}
	if corr.AmountText != nil {
		amount, ok := verifyAmount(p.Text, *corr.AmountText)
		if !ok {
			return tmpl.Render(lang, "correct_failed", map[string]any{
				"Field": fieldName("amount_minor", lang),
			}), writtenIDs, nil
		}
		update.AmountMinor = &amount
	}
	update.OccurredAt = correctedAt

	if update.CategoryID != nil || update.Merchant != nil || update.AmountMinor != nil || update.OccurredAt != nil {
		updated, err := h.txs.Update(ctx, target.ID, update)
		if err != nil {
			var fieldErr *apperror.Error
			if errors.As(err, &fieldErr) && fieldErr.Kind == apperror.KindValidation {
				return tmpl.Render(lang, "correct_failed", map[string]any{
					"Field": fieldLabel(fieldErr, lang),
				}), writtenIDs, nil
			}
			return "", nil, fmt.Errorf("correct transaction: %w", err)
		}
		target = *updated
		// The row changed, so the message that changed it counts as
		// having touched the ledger.
		writtenIDs = append(writtenIDs, target.ID)
	} else if len(writtenIDs) == 0 {
		// Nothing identifiable to change.
		return tmpl.Render(lang, "unknown", nil), nil, nil
	}

	category := valueOr(target.CategoryName, "")
	if category == "" {
		category = tmpl.Render(lang, "uncategorized", nil)
	}
	occurred := target.OccurredAt.In(service.Jakarta())
	return tmpl.Render(lang, "corrected", map[string]any{
		"Amount": money.FormatIDR(target.AmountMinor), "Merchant": valueOr(target.Merchant, ""),
		"Category": category, "Source": transactionSourceName(&target),
		"Destination": transactionDestinationName(&target), "Type": target.Type,
		"Backdated": occurred.Format("2006-01-02") != service.JakartaDate(),
		"Date":      formatDay(occurred, lang),
	}), writtenIDs, nil
}

// rewriteSource reverses a transaction and writes it again against a
// different Source, then repoints the chat batch at the new row so a
// later "batal" still finds it.
func (h *Harness) rewriteSource(
	ctx context.Context,
	p *waga.IncomingWebhookPayload,
	target *entity.Transaction,
	sourceID uuid.UUID,
	state *repository.ChatState,
) (*entity.Transaction, error) {
	occurred := target.OccurredAt
	externalID := p.MessageId + "#1"
	in := service.CreateInput{
		Type:               target.Type,
		OccurredAt:         &occurred,
		AmountMinor:        target.AmountMinor,
		SourceID:           sourceID,
		CategoryID:         target.CategoryID,
		Note:               target.Note,
		Merchant:           target.Merchant,
		Origin:             botOrigin,
		CategorySource:     target.CategorySource,
		CategoryConfidence: target.CategoryConfidence,
		ReviewStatus:       target.ReviewStatus,
		ExternalID:         &externalID,
	}
	if target.Type == "transfer" {
		dest := transactionDestinationID(target)
		if dest == nil || *dest == sourceID {
			// The re-write would be a transfer to itself; refuse before
			// touching the original.
			return nil, apperror.Validation("transaction validation failed").
				WithDetails(apperror.FieldError{
					Field: "source_id", Rule: "distinct", Message: "destination must differ from source",
				})
		}
		in.DestinationSourceID = dest
	}

	note := "koreksi sumber via WhatsApp"
	if _, err := h.txs.Reverse(ctx, target.ID, &note, botOrigin); err != nil {
		return nil, fmt.Errorf("reverse for source correction: %w", err)
	}
	created, err := h.txs.Create(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("rewrite with new source: %w", err)
	}

	if state.LastBatchMessageID != nil {
		batch, err := h.inbound.Get(ctx, *state.LastBatchMessageID)
		if err != nil {
			return nil, fmt.Errorf("load batch: %w", err)
		}
		if batch != nil {
			ids := make([]uuid.UUID, 0, len(batch.TransactionIDs))
			for _, id := range batch.TransactionIDs {
				if id == target.ID {
					ids = append(ids, created.ID)
					continue
				}
				ids = append(ids, id)
			}
			if err := h.inbound.SetTransactionIDs(ctx, batch.MessageID, ids); err != nil {
				return nil, fmt.Errorf("repoint batch: %w", err)
			}
		}
	}
	return created, nil
}

// balance answers a balance question. Confirmations never quote a
// balance; this is the one reply that does, because the user asked.
func (h *Harness) balance(ctx context.Context, sourceID *uuid.UUID, lang string) (string, error) {
	sources, err := h.sources.List(ctx, false)
	if err != nil {
		return "", fmt.Errorf("list sources: %w", err)
	}
	balances, err := h.sources.Balances(ctx)
	if err != nil {
		return "", fmt.Errorf("load balances: %w", err)
	}
	if sourceID != nil {
		src := findSource(*sourceID, sources)
		if src != nil {
			return tmpl.Render(lang, "balance_one", map[string]any{
				"Source": src.Name, "Balance": money.FormatIDR(balances[src.ID]),
			}), nil
		}
	}

	type balanceItem struct{ Source, Balance string }
	items := make([]balanceItem, 0, len(sources))
	for _, s := range sources {
		items = append(items, balanceItem{Source: s.Name, Balance: money.FormatIDR(balances[s.ID])})
	}
	overview, err := h.over.Build(ctx)
	if err != nil {
		return "", fmt.Errorf("overview: %w", err)
	}
	return tmpl.Render(lang, "balance_all", map[string]any{
		"Items": items, "Sts": money.FormatIDR(overview.StsMinor),
	}), nil
}

// verifyAmount is the money law for anything the router touched: the
// token must parse under the deterministic grammar AND appear in the
// message the user actually sent.
func verifyAmount(text, amountText string) (int64, bool) {
	token := strings.TrimSpace(amountText)
	if token == "" {
		return 0, false
	}
	amount, ok := money.ParseToken(token)
	if !ok || amount <= 0 {
		return 0, false
	}
	if !strings.Contains(squash(text), squash(token)) {
		return 0, false
	}
	return amount, true
}

// squash folds case and spaces so "Rp 25.000" in the message matches
// "rp25.000" in the answer.
func squash(s string) string {
	return strings.ReplaceAll(strings.ToLower(s), " ", "")
}

// fieldLabels translate the ledger's field names into the words the user
// sees. An unmapped field falls back to the raw name.
var fieldLabels = map[string]map[string]string{
	"id": {"category_id": "kategori", "amount_minor": "nominal", "source_id": "sumber", "occurred_at": "tanggal"},
	"en": {"category_id": "category", "amount_minor": "amount", "source_id": "source", "occurred_at": "date"},
}

func fieldLabel(err *apperror.Error, lang string) string {
	if len(err.Details) == 0 {
		return fieldName("", lang)
	}
	return fieldName(err.Details[0].Field, lang)
}

func fieldName(field, lang string) string {
	labels, ok := fieldLabels[lang]
	if !ok {
		labels = fieldLabels["id"]
	}
	if label, ok := labels[field]; ok {
		return label
	}
	return field
}

// currentSourceID is the party a transaction is spent from: the negative
// source leg, or the positive one for income.
func currentSourceID(tx *entity.Transaction) uuid.UUID {
	for i := range tx.Legs {
		leg := &tx.Legs[i]
		if leg.SourceID == nil {
			continue
		}
		if tx.Type == "transfer" && leg.SignedAmount > 0 {
			continue
		}
		return *leg.SourceID
	}
	return uuid.Nil
}

func transactionSourceName(tx *entity.Transaction) string {
	for i := range tx.Legs {
		leg := &tx.Legs[i]
		if leg.SourceID == nil || leg.SourceName == nil {
			continue
		}
		if tx.Type == "transfer" && leg.SignedAmount > 0 {
			continue
		}
		return *leg.SourceName
	}
	return ""
}

func transactionDestinationName(tx *entity.Transaction) string {
	if tx.Type != "transfer" {
		return ""
	}
	for i := range tx.Legs {
		leg := &tx.Legs[i]
		if leg.SourceID != nil && leg.SourceName != nil && leg.SignedAmount > 0 {
			return *leg.SourceName
		}
	}
	return ""
}

func transactionDestinationID(tx *entity.Transaction) *uuid.UUID {
	for i := range tx.Legs {
		leg := &tx.Legs[i]
		if leg.SourceID != nil && leg.SignedAmount > 0 {
			return leg.SourceID
		}
	}
	return nil
}

func valueOr(s *string, alt string) string {
	if s == nil || *s == "" {
		return alt
	}
	return *s
}
