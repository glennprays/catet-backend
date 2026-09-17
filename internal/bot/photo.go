package bot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/glennprays/catet-backend/internal/bot/tmpl"
	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/llm"
	"github.com/glennprays/catet-backend/internal/money"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/glennprays/catet-backend/internal/service"
	waga "github.com/glennprays/whatsapp-gateway-sdk-go"
	"github.com/google/uuid"
)

// receiptHoldLines caps how much of a long receipt the chat reply shows;
// the rest is one "and N more" line and the dashboard's problem.
const receiptHoldLines = 6

// photo turns one inbound picture into a parked receipt draft. A photo
// never writes: the printed total becomes N ledger lines only after the
// user confirms, in chat or in the dashboard (spec §6). Money comes from
// the printed TOTAL line and nothing else.
//
// Process already stamped 👀 and started typing; photo() only owns the
// outcome, because reading a receipt is slow enough that the emoji is
// the whole status display.
func (h *Harness) photo(ctx context.Context, p *waga.IncomingWebhookPayload, lang string) (string, []uuid.UUID, string, error) {
	reply, ids, emoji, err := h.readPhoto(ctx, p, lang)
	if err != nil {
		h.react(ctx, p.Chat, p.MessageId, p.From, reactRejected)
		return "", nil, lang, err
	}
	h.react(ctx, p.Chat, p.MessageId, p.From, emoji)
	return reply, ids, lang, nil
}

// readPhoto is the pipeline itself; it returns the reaction that
// describes its outcome so photo() can stamp it in one place.
func (h *Harness) readPhoto(
	ctx context.Context, p *waga.IncomingWebhookPayload, lang string,
) (string, []uuid.UUID, string, error) {
	caption := strings.TrimSpace(p.Media.Caption)

	data, mime, sha, err := fetchPhoto(ctx, h.http, p.Media)
	if err != nil {
		h.log.Warn(traceOf(ctx), "photo fetch failed", map[string]any{
			"error": err.Error(), "url_host": hostOf(p.Media.Url), "mime": p.Media.MimeType,
		})
		switch {
		case errors.Is(err, errMediaTooLarge):
			return tmpl.Render(lang, "photo_too_large", nil), nil, reactRejected, nil
		case errors.Is(err, errMediaType):
			return tmpl.Render(lang, "unsupported_media", nil), nil, reactRejected, nil
		default:
			return tmpl.Render(lang, "photo_unreadable", map[string]any{"Issue": "download"}), nil, reactRejected, nil
		}
	}

	prev, err := h.inbound.FindRepliedByMediaSha256(ctx, sha, time.Now().UTC().Add(-photoDuplicateWindow))
	if err != nil {
		return "", nil, "", fmt.Errorf("find duplicate photo: %w", err)
	}
	if prev != nil && prev.MessageID != p.MessageId {
		summary := ""
		if prev.ReplyText != nil {
			summary = firstLine(*prev.ReplyText)
		}
		return tmpl.Render(lang, "photo_duplicate", map[string]any{"Summary": summary}), nil, reactRejected, nil
	}

	if !h.llm.Enabled(ctx) {
		return tmpl.Render(lang, "photo_llm_off", nil), nil, reactRejected, nil
	}

	cats, err := h.categories.List(ctx, false)
	if err != nil {
		return "", nil, "", fmt.Errorf("list categories: %w", err)
	}
	sources, err := h.sources.List(ctx, false)
	if err != nil {
		return "", nil, "", fmt.Errorf("list sources: %w", err)
	}

	started := time.Now()
	read, err := h.llm.ReadReceipt(ctx, data, mime, caption, catCandidates(cats))
	if err != nil {
		h.log.Warn(traceOf(ctx), "photo read failed", map[string]any{"error": err.Error()})
		return tmpl.Render(lang, "error_generic", nil), nil, reactRejected, nil
	}

	// The bytes' own fingerprint: a forwarded duplicate must replay the
	// earlier answer instead of paying for a second vision call.
	// Recorded only once the model answered: a read that failed on our
	// side must be retryable, not replayed as "already sent".
	if err := h.inbound.SetMediaSha256(ctx, p.MessageId, sha); err != nil {
		h.log.Warn(traceOf(ctx), "record media hash failed", map[string]any{"error": err.Error()})
	}

	h.log.Info(traceOf(ctx), "photo read", map[string]any{
		"merchant": read.Merchant, "total_minor": read.TotalMinor, "items": len(read.Items),
		"legible": read.Legible, "document_type": read.DocumentType,
		"image_received": read.ImageReceived, "served_model": read.ServedModel,
		"prompt_tokens": read.PromptTokens, "completion_tokens": read.CompletionTokens,
		"latency_ms": time.Since(started).Milliseconds(),
	})

	params, reject := buildReceiptDraft(read, receiptInput{
		MessageID: p.MessageId, Caption: caption,
		Now: service.JakartaNow(), Cats: cats,
	})
	if reject != nil {
		if reject.Template == "photo_vision_down" {
			h.log.Warn(traceOf(ctx), "vision model saw no image", map[string]any{
				"served_model": read.ServedModel, "prompt_tokens": read.PromptTokens,
			})
		}
		return tmpl.Render(lang, reject.Template, reject.Data), nil, reactRejected, nil
	}
	params.Chat = p.Chat
	params.From = p.From

	// The paper's spelling of the shop becomes the same canonical
	// merchant a typed capture would produce, so memory, analytics and
	// the reply all agree on one name.
	if m, err := h.merchants.Resolve(ctx, params.Merchant, ""); err != nil {
		h.log.Warn(traceOf(ctx), "merchant resolve failed", map[string]any{"error": err.Error()})
	} else if m != nil {
		params.MerchantID = &m.ID
		params.Merchant = m.Name
	}

	// The caption is the user's own instruction about this photo. It may
	// name the Source outright, or need the router to read "bayarnya
	// separuh cash" — but it never confirms: a picture is not a "ya".
	captionHint, captionAnswer := h.readCaption(ctx, caption, params, sources, cats, lang)

	pick := h.resolveReceiptSource(ctx, read, captionHint, params.MerchantID, sources)
	if pick.Source != nil {
		params.SourceID = &pick.Source.ID
		params.SourceName = pick.Source.Name
		params.SourceBasis = pick.Basis
	} else {
		params.Missing = []string{"source"}
	}

	if captionAnswer != nil {
		if captionAnswer.CategoryID != nil {
			applyReceiptCategory(params, *captionAnswer.CategoryID, cats)
		}
		if len(captionAnswer.Payments) > 1 {
			if reject := applySplit(params, captionAnswer.Payments, sources); reject != nil {
				// A caption whose split does not add up is a note, not a
				// refusal: the draft stands and the question still asks.
				h.log.Warn(traceOf(ctx), "caption split ignored", map[string]any{"reason": reject.Template})
			}
		}
	}

	// Money received in a payment screenshot is income — unless the other
	// party is one of the user's own Sources, which makes it a transfer
	// between two accounts they hold.
	if read.DocumentType == "payment_screenshot" && read.Direction == "in" {
		params.Type = "income"
		if hits := matchSources(read.Counterparty, sources); len(hits) == 1 &&
			(params.SourceID == nil || hits[0].ID != *params.SourceID) {
			params.Type = "transfer"
			params.DestinationSourceID = &hits[0].ID
		}
	}

	params.ContentHash = receiptFingerprint(params)
	if dup, err := h.findReceiptDuplicate(ctx, params); err != nil {
		h.log.Warn(traceOf(ctx), "duplicate lookup failed", map[string]any{"error": err.Error()})
	} else if dup != nil {
		params.DuplicateOf = dup
	}

	summary := tmpl.Render(lang, "receipt_summary", receiptSummaryData(params, lang))
	raw, err := jsonParams(params)
	if err != nil {
		return "", nil, "", err
	}
	action, err := h.pending.Create(ctx, "receipt", raw, summary)
	if err != nil {
		return "", nil, "", fmt.Errorf("park receipt: %w", err)
	}

	state, err := h.chats.Get(ctx, p.Chat)
	if err != nil {
		return "", nil, "", fmt.Errorf("load chat state: %w", err)
	}
	if state == nil {
		state = &repository.ChatState{Chat: p.Chat}
	}

	// One outstanding question per chat (ADR-0003): a receipt that
	// arrives while another draft is waiting is only answerable in the
	// dashboard.
	if state.OpenPendingActionID != nil {
		if err := h.saveState(ctx, state); err != nil {
			return "", nil, "", err
		}
		return summary + "\n" + tmpl.Render(lang, "hold_to_dashboard", nil), nil, reactParked, nil
	}
	expect := "yes_no"
	if len(params.Missing) > 0 {
		expect = expectFor(params.Missing)
	}
	state.OpenPendingActionID = &action.ID
	state.OpenExpect = &expect
	if err := h.saveState(ctx, state); err != nil {
		return "", nil, "", err
	}
	return tmpl.Render(lang, "receipt_hold", receiptHoldData(params, summary, sources, lang)), nil, reactParked, nil
}

// readCaption reads the words the user sent with the picture. A caption
// that is plainly a Source name is resolved by the rules alone; anything
// longer is worth one router call, which can also carry a category for
// the whole receipt or a split payment. Whatever it returns is a hint —
// the caption can never confirm the draft, so Confirm, Merchant and
// DayOffset are deliberately ignored here.
func (h *Harness) readCaption(
	ctx context.Context,
	caption string,
	params *receiptParams,
	sources []entity.Source,
	cats []entity.Category,
	lang string,
) (*entity.Source, *llm.AnswerFields) {
	if caption == "" {
		return nil, nil
	}
	if hit := resolveSourceRules(caption, sources); hit != nil {
		return hit, nil
	}
	if !h.llm.Enabled(ctx) {
		return nil, nil
	}
	in, err := h.llm.Intent(ctx, llm.IntentRequest{
		Lang:  lang,
		Text:  caption,
		Today: service.JakartaDate(),
		Open: &llm.OpenQuestion{
			Expect:  "yes_no",
			Summary: tmpl.Render(lang, "receipt_summary", receiptSummaryData(params, lang)),
		},
		Sources:    candidatesOfSources(sources),
		Categories: catCandidates(cats),
	})
	if err != nil {
		if !errors.Is(err, llm.ErrUnavailable) {
			h.log.Warn(traceOf(ctx), "caption router failed", map[string]any{"error": err.Error()})
		}
		return nil, nil
	}
	if in.Answer == nil {
		return nil, nil
	}
	var hint *entity.Source
	if in.Answer.SourceID != nil {
		hint = findSource(*in.Answer.SourceID, sources)
	}
	return hint, in.Answer
}

// applyReceiptCategory labels every line of a receipt with one category
// the user named. A label chosen by a human is not a guess: it drops the
// model's confidence and marks the write as user-sourced.
func applyReceiptCategory(p *receiptParams, id uuid.UUID, cats []entity.Category) {
	name := categoryName(id, cats)
	for i := range p.Items {
		p.Items[i].CategoryID = &id
		p.Items[i].CategoryName = name
		p.Items[i].CategoryConfidence = nil
	}
	p.CategorySource = "user"
}

// candidatesOfSources is the Source whitelist handed to the router.
func candidatesOfSources(sources []entity.Source) []llm.Candidate {
	out := make([]llm.Candidate, 0, len(sources))
	for _, s := range sources {
		hint := s.Type
		if s.IsDebt {
			hint += ", debt"
		}
		out = append(out, llm.Candidate{ID: s.ID, Name: s.Name, Hint: hint})
	}
	return out
}

// findReceiptDuplicate looks for money already in the ledger for this
// receipt: the same fingerprint (the same photo, or the same receipt
// again), else the same merchant and amount on the same day (typed by
// hand, then photographed). It is a hint on the draft, never a refusal —
// the user decides.
func (h *Harness) findReceiptDuplicate(ctx context.Context, p *receiptParams) (*uuid.UUID, error) {
	if id, err := h.txRepo.FindByContentHash(ctx, p.ContentHash); err != nil || id != nil {
		return id, err
	}
	day := p.OccurredAt.In(service.JakartaNow().Location()).Format("2006-01-02")
	start, end, err := service.JakartaDayBounds(day)
	if err != nil {
		return nil, err
	}
	if p.MerchantID == nil {
		return nil, nil
	}
	return h.txRepo.FindLikelyDuplicate(ctx, *p.MerchantID, p.TotalMinor, start, end)
}

// receiptFingerprint identifies the receipt itself, not the photo of it:
// same shop, same printed date, same printed total.
func receiptFingerprint(p *receiptParams) string {
	date := p.ReceiptDate
	if date == "" {
		date = p.OccurredAt.UTC().Format("2006-01-02")
	}
	sum := sha256.Sum256([]byte("receipt|" + strings.ToLower(strings.TrimSpace(p.Merchant)) +
		"|" + date + "|" + strconv.FormatInt(p.TotalMinor, 10)))
	return hex.EncodeToString(sum[:])
}

// receiptHoldData fills the confirmation question: the summary, the first
// few lines verbatim, and either the Source we inferred — said the way it
// was decided — or the question about the one we could not.
func receiptHoldData(p *receiptParams, summary string, sources []entity.Source, lang string) map[string]any {
	shown := p.Items
	if len(shown) > receiptHoldLines {
		shown = shown[:receiptHoldLines]
	}
	lines := make([]map[string]any, 0, len(shown))
	for _, it := range shown {
		lines = append(lines, map[string]any{
			"Name": it.Name, "Amount": money.FormatIDR(it.AmountMinor), "Category": it.CategoryName,
		})
	}
	return map[string]any{
		"Summary":   summary,
		"Items":     lines,
		"More":      len(p.Items) - len(shown),
		"Source":    p.SourceName,
		"Basis":     p.SourceBasis,
		"Sources":   sourceNames(sources),
		"AskSource": p.SourceID == nil,
	}
}

// catCandidates is the category whitelist handed to the model.
func catCandidates(cats []entity.Category) []llm.Candidate {
	out := make([]llm.Candidate, 0, len(cats))
	for _, c := range cats {
		out = append(out, llm.Candidate{ID: c.ID, Name: c.Name, Hint: parentName(c, cats)})
	}
	return out
}

// firstLine keeps a duplicate notice to one line: the earlier reply's
// own summary, not its whole item list.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
