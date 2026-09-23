package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

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

// answerEdits is everything one message settles about the open draft:
// the router's own fields plus the one thing a model may never
// contribute — an amount, which only the deterministic money grammar
// produces (ADR-0003).
type answerEdits struct {
	llm.AnswerFields
	AmountMinor *int64
	// OccurredAt is a date the deterministic grammar read out of the
	// message. It wins over the router's OccurredDate: the rules saw the
	// actual words.
	OccurredAt *time.Time
	// Choice is the 1-based index the user replied with to a numbered
	// pick-list ("2"). Rules-only: a digit needs no model call.
	Choice *int
}

// answerFiller are the words that survive a Source clause without
// meaning anything: politeness and the verb that introduced the
// payment. A message made only of these plus a yes/no is fully
// explained by the rules, so it never costs a model call.
var answerFiller = map[string]bool{
	"catat": true, "aja": true, "dong": true, "deh": true, "tolong": true,
	"please": true, "bayar": true, "bayarnya": true, "pay": true, "it": true,
	"itu": true, "tadi": true, "yang": true, "buat": true, "untuk": true,
	"kemarin": true, "oke": true, "ok": true, "sip": true,
}

// parseBareAnswer explains a message with rules alone, or gives up.
// "bayar pake BCA, ya catat" is a confirmation *and* a Source, and both
// are visible to a regex — spending seven seconds of LLM latency on it
// would be theatre. So is "dari BRI, itu kemaren tanggal 17": a Source,
// a date and nothing else. nil means the intent router has to look.
func parseBareAnswer(text, expect string, sources []entity.Source) *answerEdits {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}
	// A numbered pick-list answer is a digit, never money: it has to be
	// read before the money grammar claims it.
	if expect == "bill_choice" && len(strings.Fields(trimmed)) == 1 {
		if n, err := strconv.Atoi(trimmed); err == nil && n > 0 {
			return &answerEdits{Choice: &n}
		}
	}

	hint, rest := parse.SourceHint(trimmed)
	var src *entity.Source
	if hint != "" {
		// A Source clause we cannot resolve is exactly the alias repair
		// the router exists for.
		if src = resolveSourceRules(hint, sources); src == nil {
			return nil
		}
	}

	rest, when, dated := parse.ExtractTime(rest, service.JakartaNow())

	// One money token in the remainder is the amount; two is an
	// ambiguity only the router can read.
	var amount *int64
	var words []string
	for _, word := range strings.Fields(rest) {
		if value, ok := money.ParseToken(strings.Trim(word, ",.;:!?\"'")); ok && value > 0 {
			if amount != nil {
				return nil
			}
			amount = &value
			continue
		}
		words = append(words, word)
	}

	confirm, explained := bareConfirm(strings.Join(words, " "))
	if explained && (confirm != "" || src != nil || dated || amount != nil) {
		out := &answerEdits{AnswerFields: llm.AnswerFields{Confirm: confirm}, AmountMinor: amount}
		if src != nil {
			out.SourceID = &src.ID
		}
		if dated {
			at := when
			out.OccurredAt = &at
		}
		return out
	}
	if src == nil && !dated && amount == nil {
		if hit := resolveSourceRules(trimmed, sources); hit != nil {
			return &answerEdits{AnswerFields: llm.AnswerFields{SourceID: &hit.ID}}
		}
	}
	if expect == "amount" && len(strings.Fields(trimmed)) == 1 {
		if value, ok := money.ParseToken(trimmed); ok && value > 0 {
			return &answerEdits{AmountMinor: &value}
		}
	}
	return nil
}

// bareConfirm reads a yes or a no out of what is left of a message once
// its Source, date and amount are gone. explained is false as soon as
// one word is neither a yes/no nor filler: the rules then step aside.
// An empty confirm with explained=true is a message that edited the
// draft without accepting or rejecting it.
func bareConfirm(rest string) (string, bool) {
	confirm := ""
	for _, word := range strings.Fields(strings.ToLower(rest)) {
		word = strings.Trim(word, ",.;:!?\"'")
		if word == "" {
			continue
		}
		switch parse.CommandOf(word) {
		case parse.CmdYa:
			if confirm == "no" {
				return "", false
			}
			confirm = "yes"
		case parse.CmdTidak:
			if confirm == "yes" {
				return "", false
			}
			confirm = "no"
		default:
			if !answerFiller[word] {
				return "", false
			}
		}
	}
	return confirm, true
}

// resolveDate turns whatever date the message carried into the instant
// the transaction is booked at. A non-empty reply is a refusal: the
// caller must abort the whole message rather than record a wrong day.
func (h *Harness) resolveDate(edits *answerEdits, lang string) (*time.Time, string) {
	switch {
	case edits.OccurredAt != nil:
		return guardDate(*edits.OccurredAt, lang)
	case edits.OccurredDate != nil:
		when, err := service.JakartaNoon(*edits.OccurredDate)
		if err != nil {
			return nil, ""
		}
		return guardDate(when, lang)
	}
	return nil, ""
}

// guardDate is the one place a date is judged: the future is not a
// memory, and anything older than the backdate window is a dashboard
// job. Both are refused out loud rather than silently booked on today.
func guardDate(when time.Time, lang string) (*time.Time, string) {
	now := service.JakartaNow()
	if when.After(now) {
		return nil, tmpl.Render(lang, "date_future", nil)
	}
	if when.Before(now.AddDate(0, 0, -llm.MaxBackdateDays)) {
		return nil, tmpl.Render(lang, "date_too_old", nil)
	}
	return &when, ""
}

// applyAnswer settles the open question with everything one message
// carried: a yes or a no, the Source that paid, a split across two
// Sources, one category for every line, a corrected merchant, a
// different day. Edits are applied to the parked draft first and the
// confirmation acts on the result, so "bayar pake BCA, ya" is one round
// trip rather than two.
//
// handled=false means the message settled nothing. The question is left
// standing and the state untouched — deciding what the message was
// instead belongs to the intent stage, not here.
func (h *Harness) applyAnswer(
	ctx context.Context,
	edits *answerEdits,
	messageID string,
	state *repository.ChatState,
	lang string,
) (string, []uuid.UUID, bool, error) {
	if edits == nil {
		return "", nil, false, nil
	}
	pending, err := h.pending.GetOpen(ctx, *state.OpenPendingActionID)
	if err != nil {
		return "", nil, false, fmt.Errorf("load pending action: %w", err)
	}
	if pending == nil {
		// Resolved elsewhere (the dashboard) while the chat still pointed
		// at it.
		return "", nil, false, h.clearQuestion(ctx, state)
	}
	expect := ""
	if state.OpenExpect != nil {
		expect = *state.OpenExpect
	}

	switch pending.Kind {
	case "receipt":
		return h.answerReceipt(ctx, edits, messageID, pending, state, lang)
	case "capture", "create_transaction":
		return h.answerCapture(ctx, edits, messageID, pending, state, expect, lang)
	case "bill_payment":
		return h.answerBillPayment(ctx, edits, pending, state, expect, lang)
	case "refund":
		// A refund reverses a specific row: there is nothing to edit, only
		// a yes or a no.
		if edits.Confirm == "" || edits.SourceID != nil || edits.CategoryID != nil ||
			edits.Merchant != nil || edits.Note != nil || edits.OccurredAt != nil ||
			edits.OccurredDate != nil || len(edits.Payments) > 0 {
			return "", nil, false, nil
		}
		if edits.Confirm == "no" {
			return h.discardPending(ctx, pending, state, lang)
		}
		return h.executePending(ctx, pending, nil, messageID, state, lang, "")
	}
	return "", nil, false, nil
}

// discardPending drops a parked draft the user rejected and stamps the
// refusal back on the message that opened the question.
func (h *Harness) discardPending(
	ctx context.Context, pending *entity.PendingAction, state *repository.ChatState, lang string,
) (string, []uuid.UUID, bool, error) {
	if _, err := h.pending.Resolve(ctx, pending.ID, "discard"); err != nil {
		return "", nil, false, fmt.Errorf("discard pending action: %w", err)
	}
	// Every parked kind stores the same address fields, so one shape
	// reads them all.
	var addressed struct {
		MessageID string `json:"message_id"`
		Chat      string `json:"chat"`
		From      string `json:"from"`
	}
	if err := json.Unmarshal(pending.Params, &addressed); err == nil && addressed.Chat != "" {
		h.react(ctx, addressed.Chat, addressed.MessageID, addressed.From, reactRejected)
	}
	if err := h.clearQuestion(ctx, state); err != nil {
		return "", nil, false, err
	}
	return tmpl.Render(lang, "discarded", nil), nil, true, nil
}

// executePending writes a confirmed draft and closes the question. extra
// is appended on its own line — the one place the bot volunteers
// something it learned.
func (h *Harness) executePending(
	ctx context.Context,
	pending *entity.PendingAction,
	ov *service.PendingOverrides,
	messageID string,
	state *repository.ChatState,
	lang string,
	extra string,
) (string, []uuid.UUID, bool, error) {
	txs, err := h.Execute(ctx, pending, ov)
	if err != nil {
		return "", idsOfAll(txs), false, err
	}
	if err := h.clearQuestion(ctx, state); err != nil {
		return "", nil, false, err
	}
	ids := idsOfAll(txs)
	// The confirmed write is now the batch "batal" addresses.
	if len(ids) > 0 {
		state.LastBatchMessageID = &messageID
		if err := h.saveState(ctx, state); err != nil {
			return "", nil, false, err
		}
	}
	reply := renderExecuted(pending.Kind, txs, lang)
	if extra != "" {
		reply += "\n" + extra
	}
	return reply, ids, true, nil
}

// answerReceipt edits a parked receipt with whatever the message said,
// then acts on the confirmation. The order matters: "bayar pake BCA, ya"
// must write from BCA, not ask which Source and then accept a "ya" that
// no longer has a question.
func (h *Harness) answerReceipt(
	ctx context.Context,
	edits *answerEdits,
	messageID string,
	pending *entity.PendingAction,
	state *repository.ChatState,
	lang string,
) (string, []uuid.UUID, bool, error) {
	var params receiptParams
	if err := json.Unmarshal(pending.Params, &params); err != nil {
		return "", nil, false, fmt.Errorf("decode pending receipt: %w", err)
	}
	sources, err := h.sources.List(ctx, false)
	if err != nil {
		return "", nil, false, fmt.Errorf("list sources: %w", err)
	}

	var (
		changes   []string
		newSource *entity.Source
	)

	if len(edits.Payments) > 1 {
		if reject := applySplit(&params, edits.Payments, sources); reject != nil {
			// A split that does not add up changes nothing; the question
			// stays exactly as it was.
			return tmpl.Render(lang, reject.Template, reject.Data), nil, true, nil
		}
		newSource = findSource(params.Payments[largestPayment(params.Payments)].SourceID, sources)
		changes = append(changes, tmpl.Render(lang, "receipt_change_split", map[string]any{
			"Payments": paymentLines(params.Payments),
		}))
	} else if edits.SourceID != nil {
		hit := findSource(*edits.SourceID, sources)
		if hit == nil {
			return tmpl.Render(lang, "receipt_split_unknown_source", map[string]any{
				"Sources": sourceNames(sources),
			}), nil, true, nil
		}
		params.SourceID = &hit.ID
		params.SourceName = hit.Name
		params.SourceBasis = basisUser
		params.Payments = nil
		params.Missing = nil
		newSource = hit
		changes = append(changes, tmpl.Render(lang, "receipt_change_source", map[string]any{
			"Source": hit.Name,
		}))
	}

	if edits.CategoryID != nil {
		cats, err := h.categories.List(ctx, false)
		if err != nil {
			return "", nil, false, fmt.Errorf("list categories: %w", err)
		}
		applyReceiptCategory(&params, *edits.CategoryID, cats)
		changes = append(changes, tmpl.Render(lang, "receipt_change_category", map[string]any{
			"Category": categoryName(*edits.CategoryID, cats),
		}))
	}
	if edits.Merchant != nil {
		// The user named the shop on purpose: authoritative as typed, and
		// the draft now points at that canonical merchant.
		m, err := h.merchants.ResolveUser(ctx, *edits.Merchant)
		if err != nil {
			return "", nil, false, err
		}
		params.Merchant = *edits.Merchant
		params.MerchantID = nil
		if m != nil {
			params.Merchant = m.Name
			params.MerchantID = &m.ID
		}
		changes = append(changes, tmpl.Render(lang, "receipt_change_merchant", map[string]any{
			"Merchant": params.Merchant,
		}))
	}
	when, refusal := h.resolveDate(edits, lang)
	if refusal != "" {
		return refusal, nil, true, nil
	}
	if when != nil {
		// The printed date stays as printed; only the day the money is
		// booked on moves.
		params.OccurredAt = *when
		changes = append(changes, tmpl.Render(lang, "receipt_change_date", map[string]any{
			"Date": formatDay(params.OccurredAt.In(service.Jakarta()), lang),
		}))
	}

	if len(changes) == 0 && edits.Confirm == "" {
		return "", nil, false, nil
	}

	summary := tmpl.Render(lang, "receipt_summary", receiptSummaryData(&params, lang))
	if len(changes) > 0 {
		raw, err := jsonParams(params)
		if err != nil {
			return "", nil, false, err
		}
		if err := h.pending.UpdateParams(ctx, pending.ID, raw, summary); err != nil {
			return "", nil, false, fmt.Errorf("update pending receipt: %w", err)
		}
		// Execute reads the draft from the entity it was handed.
		pending.Params = raw
		pending.Summary = summary
	}

	if edits.Confirm == "no" {
		return h.discardPending(ctx, pending, state, lang)
	}

	// The first Source the user ever picks for a receipt becomes the
	// fallback for the next one, so the same question is not asked twice.
	learned := ""
	if src := h.learnPhotoDefault(ctx, newSource); src != nil {
		learned = tmpl.Render(lang, "receipt_default_learned", map[string]any{"Source": src.Name})
	}

	if edits.Confirm == "yes" {
		if params.SourceID == nil {
			return h.keepReceiptQuestion(ctx, state, "source",
				tmpl.Render(lang, "receipt_need_source", map[string]any{"Sources": sourceNames(sources)}))
		}
		return h.executePending(ctx, pending, nil, messageID, state, lang, learned)
	}

	expect := "yes_no"
	if params.SourceID == nil {
		expect = "source"
	}
	reply := tmpl.Render(lang, "receipt_updated", map[string]any{"Change": strings.Join(changes, " · ")})
	if learned != "" {
		reply += "\n" + learned
	}
	return h.keepReceiptQuestion(ctx, state, expect, reply)
}

// keepReceiptQuestion leaves the draft parked and re-points the chat at
// what it is now waiting for.
func (h *Harness) keepReceiptQuestion(
	ctx context.Context, state *repository.ChatState, expect, reply string,
) (string, []uuid.UUID, bool, error) {
	state.OpenExpect = &expect
	if err := h.saveState(ctx, state); err != nil {
		return "", nil, false, err
	}
	return reply, nil, true, nil
}

// largestPayment is the share the memory and the learned default speak
// for: the account the user thinks of as having paid.
func largestPayment(payments []receiptPayment) int {
	best := 0
	for i := range payments {
		if payments[i].AmountMinor > payments[best].AmountMinor {
			best = i
		}
	}
	return best
}

// paymentLines renders a split for the copy that reads it back.
func paymentLines(payments []receiptPayment) []map[string]any {
	out := make([]map[string]any, 0, len(payments))
	for _, pay := range payments {
		out = append(out, map[string]any{
			"Source": pay.SourceName, "Amount": money.FormatIDR(pay.AmountMinor),
		})
	}
	return out
}

// answerCapture applies the same edits to a parked capture. A capture
// holds one transaction, so a split payment is meaningless here and the
// missing field it was waiting for is recomputed from what arrived.
func (h *Harness) answerCapture(
	ctx context.Context,
	edits *answerEdits,
	messageID string,
	pending *entity.PendingAction,
	state *repository.ChatState,
	expect string,
	lang string,
) (string, []uuid.UUID, bool, error) {
	var params captureParams
	if err := json.Unmarshal(pending.Params, &params); err != nil {
		return "", nil, false, fmt.Errorf("decode pending capture: %w", err)
	}
	sources, err := h.sources.List(ctx, false)
	if err != nil {
		return "", nil, false, fmt.Errorf("list sources: %w", err)
	}

	var changes []string
	if edits.SourceID != nil {
		hit := findSource(*edits.SourceID, sources)
		if hit == nil {
			return "", nil, false, nil
		}
		if expect == "destination" {
			params.DestinationSourceID = &hit.ID
			params.Missing = withoutField(params.Missing, "destination")
		} else {
			params.SourceID = &hit.ID
			params.SourceName = hit.Name
			params.Missing = withoutField(params.Missing, "source")
		}
		changes = append(changes, tmpl.Render(lang, "receipt_change_source", map[string]any{
			"Source": hit.Name,
		}))
	}
	if edits.AmountMinor != nil {
		params.AmountMinor = *edits.AmountMinor
		params.Missing = withoutField(params.Missing, "amount")
		changes = append(changes, money.FormatIDR(params.AmountMinor))
	}
	if edits.CategoryID != nil {
		cats, err := h.categories.List(ctx, false)
		if err != nil {
			return "", nil, false, fmt.Errorf("list categories: %w", err)
		}
		user := "user"
		params.CategoryID = edits.CategoryID
		params.CategorySource = &user
		params.CategoryConfidence = nil
		changes = append(changes, tmpl.Render(lang, "receipt_change_category", map[string]any{
			"Category": categoryName(*edits.CategoryID, cats),
		}))
	}
	if edits.Merchant != nil {
		m, err := h.merchants.ResolveUser(ctx, *edits.Merchant)
		if err != nil {
			return "", nil, false, err
		}
		params.Merchant = strings.ToLower(strings.TrimSpace(*edits.Merchant))
		params.MerchantID = nil
		if m != nil {
			params.Merchant = m.Name
			params.MerchantID = &m.ID
		}
		changes = append(changes, tmpl.Render(lang, "receipt_change_merchant", map[string]any{
			"Merchant": params.Merchant,
		}))
	}
	if edits.Note != nil {
		params.Note = *edits.Note
		changes = append(changes, tmpl.Render(lang, "receipt_change_note", map[string]any{
			"Note": params.Note,
		}))
	}
	when, refusal := h.resolveDate(edits, lang)
	if refusal != "" {
		return refusal, nil, true, nil
	}
	if when != nil {
		params.OccurredAt = *when
		changes = append(changes, tmpl.Render(lang, "receipt_change_date", map[string]any{
			"Date": formatDay(params.OccurredAt.In(service.Jakarta()), lang),
		}))
	}

	if len(changes) == 0 && edits.Confirm == "" {
		return "", nil, false, nil
	}
	if len(changes) > 0 {
		raw, err := jsonParams(params)
		if err != nil {
			return "", nil, false, err
		}
		if err := h.pending.UpdateParams(ctx, pending.ID, raw, pending.Summary); err != nil {
			return "", nil, false, fmt.Errorf("update pending capture: %w", err)
		}
		pending.Params = raw
	}

	if edits.Confirm == "no" {
		return h.discardPending(ctx, pending, state, lang)
	}
	// A capture is written the moment nothing is missing: it was only
	// parked because of the gap the message just closed.
	if len(params.Missing) == 0 && (edits.Confirm == "yes" || len(changes) > 0) {
		return h.executePending(ctx, pending, nil, messageID, state, lang, "")
	}

	next := expectFor(params.Missing)
	return h.keepReceiptQuestion(ctx, state, next,
		tmpl.Render(lang, "receipt_updated", map[string]any{"Change": strings.Join(changes, " · ")}))
}

// withoutField drops a field from the missing list once it is supplied.
func withoutField(missing []string, field string) []string {
	out := missing[:0]
	for _, m := range missing {
		if m != field {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// clearQuestion forgets the outstanding question without touching the
// Hold Queue row behind it.
func (h *Harness) clearQuestion(ctx context.Context, state *repository.ChatState) error {
	state.OpenPendingActionID = nil
	state.OpenExpect = nil
	return h.saveState(ctx, state)
}

// startRefund parks a refund against the most recent matching expense.
// Refunds always confirm (spec §6): reversing the wrong transaction is
// worse than one extra round trip.
func (h *Harness) startRefund(
	ctx context.Context,
	p *waga.IncomingWebhookPayload,
	draft parse.Draft,
	state *repository.ChatState,
	sources []entity.Source,
	lang string,
	externalID string,
) (string, bool, error) {
	// "kembalian dari gojek" leaves the shop in a from-clause, not in
	// the merchant: any of the three names the target.
	merchant := firstNonEmpty(draft.Merchant, firstOf(draft.Froms), firstOf(draft.Places))
	if merchant == "" {
		return tmpl.Render(lang, "refund_no_target", map[string]any{"Merchant": ""}), true, nil
	}
	// Refund targets are found through the merchant the ledger actually
	// recorded, so "gojek" reaches rows written as "Gojek". Memory only:
	// asking the model to invent a merchant here would invent a target.
	canonical, err := h.merchants.Lookup(ctx, merchant)
	if err != nil {
		return "", false, fmt.Errorf("look up refund merchant: %w", err)
	}
	if canonical == nil {
		return tmpl.Render(lang, "refund_no_target", map[string]any{"Merchant": merchant}), true, nil
	}
	target, err := h.txRepo.LatestExpenseByMerchant(ctx, canonical.ID, time.Now().UTC().Add(-refundLookback))
	if err != nil {
		return "", false, fmt.Errorf("find refund target: %w", err)
	}
	if target == nil {
		return tmpl.Render(lang, "refund_no_target", map[string]any{"Merchant": merchant}), true, nil
	}

	sourceName := ""
	for _, leg := range target.Legs {
		if leg.SourceID != nil {
			sourceName = sourceNameOf(*leg.SourceID, sources)
			break
		}
	}
	params, err := jsonParams(refundParams{
		ReversesTransactionID: target.ID,
		Merchant:              merchant,
		AmountMinor:           target.AmountMinor,
		SourceName:            sourceName,
		OccurredAt:            target.OccurredAt,
		MessageID:             p.MessageId,
		ExternalID:            externalID,
		OriginalText:          p.Text,
		Chat:                  p.Chat,
		From:                  p.From,
	})
	if err != nil {
		return "", false, err
	}
	summary := "Refund " + merchant + " " + money.FormatIDR(target.AmountMinor)
	action, err := h.pending.FindOrCreateByExternalID(ctx, "refund", externalID, params, summary)
	if err != nil {
		return "", false, fmt.Errorf("park refund: %w", err)
	}
	if action.ResolvedAt != nil {
		return tmpl.Render(lang, "no_open_question", nil), true, nil
	}
	expect := "refund_target"
	state.OpenPendingActionID = &action.ID
	state.OpenExpect = &expect

	question := tmpl.Render(lang, "q_refund_target", map[string]any{
		"Merchant": merchant,
		"Amount":   money.FormatIDR(target.AmountMinor),
		"Date":     formatDay(target.OccurredAt.In(service.JakartaNow().Location()), lang),
	})
	return tmpl.Render(lang, "hold_saved", map[string]any{
		"Summary": summary, "Question": question,
	}), true, nil
}

// renderTransaction turns a freshly written transaction into the same
// confirmation the inline capture path produces.
func renderTransaction(tx *entity.Transaction, lang string) string {
	if tx == nil {
		return tmpl.Render(lang, "discarded", nil)
	}
	// Leg rows already carry their display names; for a transfer the
	// outbound (negative) leg is the source and the inbound one the
	// destination.
	var sourceName, destName string
	for _, leg := range tx.Legs {
		if leg.SourceID == nil || leg.SourceName == nil {
			continue
		}
		if tx.Type == "transfer" && leg.SignedAmount > 0 {
			destName = *leg.SourceName
			continue
		}
		sourceName = *leg.SourceName
	}

	merchant := ""
	if tx.Merchant != nil {
		merchant = *tx.Merchant
	}
	category := ""
	if tx.CategoryName != nil {
		category = *tx.CategoryName
	}
	if category == "" {
		category = tmpl.Render(lang, "uncategorized", nil)
	}

	note := ""
	if tx.Note != nil {
		note = *tx.Note
	}
	occurred := tx.OccurredAt.In(service.Jakarta())
	data := map[string]any{
		"Amount": money.FormatIDR(tx.AmountMinor), "Merchant": merchant, "Note": note,
		"Category": category, "Source": sourceName, "Destination": destName,
		"Backdated": occurred.Format("2006-01-02") != service.JakartaDate(),
		"Date":      formatDay(occurred, lang),
	}
	switch tx.Type {
	case "income":
		return tmpl.Render(lang, "recorded_income", data)
	case "transfer":
		return tmpl.Render(lang, "recorded_transfer", data)
	default:
		return tmpl.Render(lang, "recorded_one", data)
	}
}

// renderRefund confirms a reversal in the refund's own words.
func renderRefund(tx *entity.Transaction, lang string) string {
	if tx == nil {
		return tmpl.Render(lang, "discarded", nil)
	}
	// The reversal credits the source back: its source leg is positive.
	sourceName := ""
	for _, leg := range tx.Legs {
		if leg.SourceID != nil && leg.SourceName != nil && leg.SignedAmount > 0 {
			sourceName = *leg.SourceName
			break
		}
	}
	merchant := ""
	if tx.Merchant != nil {
		merchant = *tx.Merchant
	}
	return tmpl.Render(lang, "refund_done", map[string]any{
		"Merchant": merchant, "Amount": money.FormatIDR(tx.AmountMinor),
		"Source": sourceName,
	})
}

// renderExecuted confirms whatever the pending action wrote, in that
// kind's own words. An action that wrote nothing was discarded.
func renderExecuted(kind string, txs []*entity.Transaction, lang string) string {
	if len(txs) == 0 {
		return tmpl.Render(lang, "discarded", nil)
	}
	switch kind {
	case "refund":
		return renderRefund(txs[0], lang)
	case "receipt":
		return renderReceiptWritten(txs, lang)
	default:
		return renderTransaction(txs[0], lang)
	}
}

// renderReceiptWritten confirms the whole batch a receipt wrote: how many
// lines, the total they add up to and where the money left from. The
// total is the sum of what was actually written, not what was parked.
func renderReceiptWritten(txs []*entity.Transaction, lang string) string {
	var total int64
	merchant, sourceName := "", ""
	for _, tx := range txs {
		total += tx.AmountMinor
		if merchant == "" && tx.Merchant != nil {
			merchant = *tx.Merchant
		}
		if sourceName != "" {
			continue
		}
		for _, leg := range tx.Legs {
			if leg.SourceID != nil && leg.SourceName != nil && leg.SignedAmount < 0 {
				sourceName = *leg.SourceName
				break
			}
		}
	}
	return tmpl.Render(lang, "receipt_recorded", map[string]any{
		"N": len(txs), "Merchant": merchant,
		"Total": money.FormatIDR(total), "Source": sourceName,
	})
}

// idsOfAll is the batch pointer "batal" reverses.
func idsOfAll(txs []*entity.Transaction) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(txs))
	for _, tx := range txs {
		if tx != nil {
			out = append(out, tx.ID)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
