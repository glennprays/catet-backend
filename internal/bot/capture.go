package bot

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/glennprays/catet-backend/internal/bot/parse"
	"github.com/glennprays/catet-backend/internal/bot/tmpl"
	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/money"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/glennprays/catet-backend/internal/service"
	waga "github.com/glennprays/whatsapp-gateway-sdk-go"
	"github.com/google/uuid"
)

// captureParams is the Hold Queue payload of an unfinished capture. It
// carries everything the dashboard needs to finish the write without
// re-reading the chat, plus the original text so the user recognises it.
type captureParams struct {
	Type                string     `json:"type"`
	AmountMinor         int64      `json:"amount_minor"`
	Merchant            string     `json:"merchant,omitempty"`
	MerchantID          *uuid.UUID `json:"merchant_id,omitempty"`
	Note                string     `json:"note,omitempty"`
	SourceID            *uuid.UUID `json:"source_id,omitempty"`
	SourceName          string     `json:"source_name,omitempty"`
	DestinationSourceID *uuid.UUID `json:"destination_source_id,omitempty"`
	CategoryID          *uuid.UUID `json:"category_id,omitempty"`
	CategorySource      *string    `json:"category_source,omitempty"`
	CategoryConfidence  *float64   `json:"category_confidence,omitempty"`
	OccurredAt          time.Time  `json:"occurred_at"`
	MessageID           string     `json:"message_id"`
	ExternalID          string     `json:"external_id,omitempty"`
	OriginalText        string     `json:"original_text"`
	Missing             []string   `json:"missing"`
	// Chat and From address the message that opened this question, so a
	// later resolution can stamp the original with its outcome.
	Chat string `json:"chat,omitempty"`
	From string `json:"from,omitempty"`
}

// refundParams is the Hold Queue payload of a refund awaiting its yes/no.
type refundParams struct {
	ReversesTransactionID uuid.UUID `json:"reverses_transaction_id"`
	Merchant              string    `json:"merchant"`
	AmountMinor           int64     `json:"amount_minor"`
	SourceName            string    `json:"source_name"`
	OccurredAt            time.Time `json:"occurred_at"`
	MessageID             string    `json:"message_id"`
	OriginalText          string    `json:"original_text"`
	Chat                  string    `json:"chat,omitempty"`
	From                  string    `json:"from,omitempty"`
}

// writtenItem is one confirmed ledger line, ready for the reply.
type writtenItem struct {
	N           int
	Amount      string
	Merchant    string
	Note        string
	Category    string
	Source      string
	Destination string
	Type        string
	Backdated   bool
	Date        string
}

// draft is one candidate the write path accepts. The embedded
// parse.Draft is the deterministic reading of the text; ids from the
// intent stage are whitelist-checked hints, while parser hints remain
// authoritative when present.
type draft struct {
	parse.Draft
	SourceID           *uuid.UUID
	DestinationID      *uuid.UUID
	CategoryID         *uuid.UUID
	SourceFromParser   bool
}

// wrapDrafts lifts the parser's output into the write path with nothing
// pre-resolved.
func wrapDrafts(in []parse.Draft) []draft {
	out := make([]draft, 0, len(in))
	for _, d := range in {
		out = append(out, draft{Draft: d, SourceFromParser: true})
	}
	return out
}

// capture resolves every draft, writes what the rules fully decided and
// parks the rest. At most one clarifying question leaves the bot per
// message (ADR-0003); a second unresolved draft goes straight to the
// dashboard.
func (h *Harness) capture(
	ctx context.Context,
	p *waga.IncomingWebhookPayload,
	drafts []draft,
	state *repository.ChatState,
	lang string,
	sourceFooter string,
) (string, []uuid.UUID, string, error) {
	sources, err := h.sources.List(ctx, false)

	if err != nil {
		return "", nil, lang, fmt.Errorf("list sources: %w", err)
	}
	var footerSource *entity.Source
	if strings.TrimSpace(sourceFooter) != "" {
		// A shared footer is resolved once, deterministically. An
		// unmatched or ambiguous hint intentionally stays nil so every
		// affected draft reaches the normal Source Hold below.
		footerSource = resolveSourceRules(sourceFooter, sources)
	}
	cats, err := h.categories.List(ctx, false)
	if err != nil {
		return "", nil, lang, fmt.Errorf("list categories: %w", err)
	}

	// One choke point for both capture paths: a future date is not a
	// memory and a year-old one is a dashboard job, so neither is written
	// or parked.
	for _, d := range drafts {
		if _, refusal := guardDate(d.OccurredAt, lang); refusal != "" {
			return refusal, nil, lang, nil
		}
	}

	var (
		written   []writtenItem
		txIDs     []uuid.UUID
		questions []string
		deferred  int
		today     = service.JakartaDate()
	)

	askOrDefer := func(summary, question string, params captureParams) error {
		raw, err := jsonParams(params)
		if err != nil {
			return err
		}
		action, err := h.pending.Create(ctx, "capture", raw, summary)
		if err != nil {
			return fmt.Errorf("park capture: %w", err)
		}
		// The first unresolved draft owns the chat's single question; any
		// further one is only answerable in the dashboard.
		if state.OpenPendingActionID == nil && question != "" {
			expect := expectFor(params.Missing)
			state.OpenPendingActionID = &action.ID
			state.OpenExpect = &expect
			questions = append(questions, tmpl.Render(lang, "hold_saved", map[string]any{
				"Summary": summary, "Question": question,
			}))
			return nil
		}
		deferred++
		return nil
	}

	for i, d := range drafts {
		n := i + 1

		// Source first, even for a draft the parser already refused: a
		// parked capture that already knows its Source only has to ask
		// about the one thing that is actually missing.
		src, places, explicit := h.draftSource(ctx, d, sources)
		footerAffected := strings.TrimSpace(sourceFooter) != "" && !parserSourceExplicit(d, sources)
		if footerAffected {
			// A shared footer wins over model hints. A nil result is
			// intentional: it suppresses memory/default inference.
			src = footerSource
			explicit = true
		}

		// The first place is where the money went ("dari alfamart"); what
		// the user typed around it is the note ("tissu").
		merchant, note := d.Merchant, ""
		if len(places) > 0 {
			merchant = places[0]
			note = strings.TrimSpace(strings.Join(append([]string{d.Merchant}, places[1:]...), " "))
		}

		if !explicit {
			if src == nil {
				// "bensin 20k cash": a bare Source name trailing the text.
				if trailing, rest := splitTrailingWord(d.Merchant); trailing != "" {
					if hit := resolveSourceRules(trailing, sources); hit != nil {
						src = hit
						if len(places) > 0 {
							note = strings.TrimSpace(strings.Join(append([]string{rest}, places[1:]...), " "))
						} else {
							merchant = rest
						}
					}
				}
			}
		}

		// The canonical merchant is resolved before the memory lookups:
		// both the Source memory and the category memory are keyed on the
		// entity, so "alfamart" and "Alfamart Kebon Jeruk" share one.
		var canonical *entity.Merchant
		if merchant != "" {
			m, err := h.merchants.Resolve(ctx, merchant, note)
			if err != nil {
				h.log.Warn(traceOf(ctx), "merchant resolve failed", map[string]any{"error": err.Error()})
			} else if m != nil {
				canonical = m
				// The phrase the user actually typed survives as the note
				// when the canonical name no longer contains it.
				if note == "" && repository.AliasKey(merchant) != repository.AliasKey(m.Name) {
					note = merchant
				}
				merchant = m.Name
			}
		}

		if !explicit {
			if src == nil && canonical != nil {
				// The same memory a receipt uses: this shop was paid from
				// there last time, so a typed "alfamart 15k" needs no
				// question either.
				if id, err := h.txRepo.LastSourceForMerchant(ctx, canonical.ID); err != nil {
					h.log.Warn(traceOf(ctx), "source memory lookup failed", map[string]any{"error": err.Error()})
				} else if id != nil {
					src = findSource(*id, sources)
				}
			}
			if src == nil {
				src = defaultSource(sources)
			}
		}

		// A hold that will be written on another day says so: the user
		// answering "dari BRI" two days later must see which day they are
		// confirming.
		dateLabel := ""
		if d.OccurredAt.In(service.Jakarta()).Format("2006-01-02") != today {
			dateLabel = formatDay(d.OccurredAt, lang)
		}

		params := captureParams{
			Type: d.Type, AmountMinor: d.AmountMinor, Merchant: merchant, Note: note,
			OccurredAt: d.OccurredAt, MessageID: p.MessageId, OriginalText: p.Text,
			Chat: p.Chat, From: p.From,
		}
		if canonical != nil {
			params.MerchantID = &canonical.ID
		}
		if src != nil {
			params.SourceID = &src.ID
			params.SourceName = src.Name
		}
		if d.CategoryID != nil && (d.Type == "expense" || d.Type == "income") {
			params.CategoryID = d.CategoryID
			llmSource := "llm"
			params.CategorySource = &llmSource
		}

		if d.Hold != nil {
			params.Missing = missingFor(d.Hold.Code)
			if footerAffected && src == nil {
				params.Missing = appendMissing(params.Missing, "source")
			}
			question := tmpl.Render(lang, d.Hold.Question, map[string]any{"Sources": sourceNames(sources)})
			if err := askOrDefer(summaryOf(merchant, note, d.AmountMinor, d.Type, dateLabel), question, params); err != nil {
				return "", nil, lang, err
			}
			continue
		}

		if d.Type == "refund" {
			reply, handled, err := h.startRefund(ctx, p, d.Draft, state, sources, lang)
			if err != nil {
				return "", nil, lang, err
			}
			if handled {
				questions = append(questions, reply)
			}
			continue
		}

		if src == nil {
			params.Missing = []string{"source"}
			question := tmpl.Render(lang, "q_source", map[string]any{"Sources": sourceNames(sources)})
			if err := askOrDefer(summaryOf(merchant, note, d.AmountMinor, d.Type, dateLabel), question, params); err != nil {
				return "", nil, lang, err
			}
			continue
		}

		var dest *entity.Source
		if d.Type == "transfer" {
			if d.DestinationID != nil {
				dest = findSource(*d.DestinationID, sources)
			}
			if dest == nil {
				dest = h.resolveSource(ctx, d.DestHint, sources)
			}
			if dest == nil || dest.ID == src.ID {
				params.Missing = []string{"destination"}
				question := tmpl.Render(lang, "q_destination", map[string]any{"Sources": sourceNames(sources)})
				if err := askOrDefer(summaryOf(merchant, note, d.AmountMinor, d.Type, dateLabel), question, params); err != nil {
					return "", nil, lang, err
				}
				continue
			}
		}

		// A category the intent stage already resolved is the user's own
		// wording matched against the whitelist; re-guessing it would only
		// add a round trip.
		verdict := categoryVerdict{ReviewStatus: "clean"}
		switch {
		case d.CategoryID != nil && (d.Type == "expense" || d.Type == "income"):
			llmSource := "llm"
			verdict = categoryVerdict{
				ID: d.CategoryID, Name: categoryName(*d.CategoryID, cats),
				Source: &llmSource, ReviewStatus: "clean",
			}
		case d.Type == "expense" || d.Type == "income":
			verdict = h.resolveCategory(ctx, params.MerchantID, merchant, note, d.Type, cats)
		}

		occurred := d.OccurredAt
		externalID := fmt.Sprintf("%s#%d", p.MessageId, n)
		hash := contentHash(p.Text, today)
		in := service.CreateInput{
			Type:               d.Type,
			OccurredAt:         &occurred,
			AmountMinor:        d.AmountMinor,
			SourceID:           src.ID,
			CategoryID:         verdict.ID,
			Origin:             botOrigin,
			CategorySource:     verdict.Source,
			CategoryConfidence: verdict.Confidence,
			ReviewStatus:       verdict.ReviewStatus,
			ExternalID:         &externalID,
			ContentHash:        &hash,
		}
		if merchant != "" {
			m := merchant
			in.Merchant = &m
		}
		in.MerchantID = params.MerchantID
		if note != "" {
			nt := note
			in.Note = &nt
		}
		if dest != nil {
			in.DestinationSourceID = &dest.ID
		}

		tx, err := h.txs.Create(ctx, in)
		if err != nil {
			if errIsConflict(err) {
				// The same message already wrote this line (a replayed
				// delivery that crashed after the insert).
				h.log.Warn(traceOf(ctx), "duplicate capture ignored", map[string]any{"external_id": externalID})
				continue
			}
			return "", nil, lang, fmt.Errorf("write capture: %w", err)
		}
		txIDs = append(txIDs, tx.ID)

		item := writtenItem{
			N: len(written) + 1, Amount: money.FormatIDR(d.AmountMinor),
			Merchant: merchant, Note: note, Source: src.Name, Type: d.Type,
			Category: verdict.Name,
		}
		if item.Category == "" {
			item.Category = tmpl.Render(lang, "uncategorized", nil)
		}
		if dest != nil {
			item.Destination = dest.Name
		}
		if occurred.In(service.Jakarta()).Format("2006-01-02") != today {
			item.Backdated = true
			item.Date = formatDay(occurred, lang)
		}
		written = append(written, item)
	}

	if len(txIDs) > 0 {
		state.LastBatchMessageID = &p.MessageId
	}
	if err := h.saveState(ctx, state); err != nil {
		return "", nil, lang, err
	}

	parts := make([]string, 0, 3)
	if body := renderWritten(written, lang); body != "" {
		parts = append(parts, body)
	}
	parts = append(parts, questions...)
	if deferred > 0 {
		parts = append(parts, tmpl.Render(lang, "hold_to_dashboard", nil))
	}
	return strings.Join(parts, "\n"), txIDs, lang, nil
}

// renderWritten turns the confirmed lines into the reply body. It reads
// nothing beyond the write itself: a confirmation states what was
// recorded, never the account state.
func renderWritten(written []writtenItem, lang string) string {
	if len(written) == 0 {
		return ""
	}
	if len(written) == 1 {
		it := written[0]
		data := map[string]any{
			"Amount": it.Amount, "Merchant": it.Merchant, "Note": it.Note,
			"Category": it.Category, "Source": it.Source, "Destination": it.Destination,
			"Backdated": it.Backdated, "Date": it.Date,
		}
		switch it.Type {
		case "income":
			return tmpl.Render(lang, "recorded_income", data)
		case "transfer":
			return tmpl.Render(lang, "recorded_transfer", data)
		default:
			return tmpl.Render(lang, "recorded_one", data)
		}
	}
	return tmpl.Render(lang, "recorded_many", map[string]any{"Items": written})
}

// summaryOf is the Hold Queue row's one-line label: where the money
// went, what it was for, how much, and — only when it is not today —
// which day.
func summaryOf(merchant, note string, amountMinor int64, typ, date string) string {
	label := merchant
	if label == "" {
		label = typ
	}
	if note != "" {
		label += " · " + note
	}
	if amountMinor > 0 {
		label += " " + money.FormatIDR(amountMinor)
	}
	if date != "" {
		label += " · " + date
	}
	return label
}

// missingFor maps a parser hold to the params.missing list the dashboard
// renders.
func missingFor(code string) []string {
	switch code {
	case "no_amount", "ambiguous_amounts":
		return []string{"amount"}
	case "no_destination":
		return []string{"destination"}
	default:
		return []string{code}
	}
}

// expectFor maps the missing field to the answer the next message may
// carry.
func expectFor(missing []string) string {
	if len(missing) == 0 {
		return "yes_no"
	}
	switch missing[0] {
	case "amount":
		return "amount"
	case "destination":
		return "destination"
	case "source":
		return "source"
	default:
		return "yes_no"
	}
}

// splitTrailingWord returns the last word of s and the remainder, so a
// bare Source name at the end of a capture can be tested against the
// whitelist ("bensin 20k cash").
func splitTrailingWord(s string) (string, string) {
	fields := strings.Fields(s)
	if len(fields) < 2 {
		return "", s
	}
	return fields[len(fields)-1], strings.Join(fields[:len(fields)-1], " ")
}

func parserSourceExplicit(d draft, sources []entity.Source) bool {
	if !d.SourceFromParser {
		return false
	}
	if len(d.Pays) > 0 {
		return true
	}
	// draftSource intentionally treats refund "dari" clauses as merchant
	// text, so they cannot suppress a shared footer Source.
	if d.Type == parse.TypeRefund {
		return false
	}
	for _, word := range d.Froms {
		if resolveSourceRules(word, sources) != nil {
			return true
		}
	}
	return false
}

func appendMissing(missing []string, field string) []string {
	for _, existing := range missing {
		if existing == field {
			return missing
		}
	}
	return append(missing, field)
}
