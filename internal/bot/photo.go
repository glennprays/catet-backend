package bot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

// receiptHoldLines caps how much of a long receipt the chat reply shows;
// the rest is one "and N more" line and the dashboard's problem.
const (
	receiptHoldLines         = 6
	receiptPendingActionKind = "receipt"
)

// receiptPendingExternalID is separate from receipt line external IDs:
// accepting a photo only parks one draft, so retries must reuse that draft
// before the user confirms it.
func receiptPendingExternalID(messageID string) string {
	return messageID + "#" + receiptPendingActionKind
}

// photo turns one inbound picture into a parked receipt draft. A photo
// never writes: the printed total becomes N ledger lines only after the
// user confirms, in chat or in the dashboard (spec §6). Money comes from
// the printed TOTAL line and nothing else.
//
// Process stamps 👀 before work and the harness stamps the persisted
// outcome after successful delivery, so photo() has no delivery side effects.
func (h *Harness) photo(ctx context.Context, p *waga.IncomingWebhookPayload, lang string) (string, []uuid.UUID, string, error) {
	reply, ids, _, err := h.readPhoto(ctx, p, lang)
	return reply, ids, lang, err
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
	// The bytes' own fingerprint is durable as soon as download has
	// succeeded, before duplicate checks or vision. A vision failure
	// must still leave the exact fetched media addressable for recovery.
	if err := h.inbound.SetMediaSha256(ctx, p.MessageId, sha); err != nil {
		h.log.Warn(traceOf(ctx), "record media hash failed", map[string]any{"error": err.Error()})
		return "", nil, "", fmt.Errorf("media_hash_persist_failed")
	}

	prev, err := h.inbound.FindRepliedByMediaSha256(ctx, sha, time.Now().UTC().Add(-photoDuplicateWindow))
	if err != nil {
		return "", nil, "", fmt.Errorf("find duplicate photo: %w", err)
	}
	if prev != nil && prev.MessageID != p.MessageId && !readFailed(prev) {
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
	if modelCtx, cancel, ok := h.auxiliaryModelContext(ctx); ok {
		m, err := h.merchants.ResolveWithModelContext(ctx, modelCtx, params.Merchant, "")
		cancel()
		if err != nil {
			h.log.Warn(traceOf(ctx), "merchant resolve degraded", map[string]any{"error": err.Error()})
		} else if m != nil {
			params.MerchantID = &m.ID
			params.Merchant = m.Name
		}
	} else if m, err := h.merchants.ResolveUser(ctx, params.Merchant); err != nil {
		h.log.Warn(traceOf(ctx), "merchant resolve failed", map[string]any{"error": err.Error()})
	} else if m != nil {
		params.MerchantID = &m.ID
		params.Merchant = m.Name
	}

	// The caption is the user's own instruction about this photo. It may
	// name the Source outright, or need the router to read "bayarnya
	// separuh cash" — but it never confirms: a picture is not a "ya".
	captionRead := h.readCaption(ctx, caption, params, sources, cats, lang)

	pick := h.resolveReceiptSource(ctx, read, captionRead.Source, params.MerchantID, sources)
	if pick.Source != nil {
		params.SourceID = &pick.Source.ID
		params.SourceName = pick.Source.Name
		params.SourceBasis = pick.Basis
	} else {
		params.Missing = []string{"source"}
	}

	if captionRead.CategoryID != nil {
		applyReceiptCategory(params, *captionRead.CategoryID, cats)
	}
	if captionRead.Answer != nil && len(captionRead.Answer.Payments) > 1 {
		if reject := applySplit(params, captionRead.Answer.Payments, sources); reject != nil {
			// A caption whose split does not add up is a note, not a
			// refusal: the draft stands and the question still asks.
			h.log.Warn(traceOf(ctx), "caption split ignored", map[string]any{"reason": reject.Template})
		}
	}
	params.Unread = captionRead.Unread

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
	raw, err := jsonParams(struct {
		*receiptParams
		ExternalID string `json:"external_id"`
	}{receiptParams: params, ExternalID: receiptPendingExternalID(p.MessageId)})
	if err != nil {
		return "", nil, "", err
	}
	action, err := h.pending.FindOrCreateByExternalID(
		ctx, receiptPendingActionKind, receiptPendingExternalID(p.MessageId), raw, summary,
	)
	if err != nil {
		return "", nil, "", fmt.Errorf("park receipt: %w", err)
	}
	if len(action.Params) > 0 {
		var stored receiptParams
		if err := json.Unmarshal(action.Params, &stored); err == nil && stored.MessageID != "" {
			// A durable retry must show and confirm the exact draft that
			// created the action, never a changed vision/caption result.
			params = &stored
			summary = action.Summary
		}
	}
	if action.ResolvedAt != nil {
		return tmpl.Render(lang, "no_open_question", nil), nil, reactNone, nil
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

// readFailed reports whether an earlier delivery of these exact bytes
// ended in a failure the user was told to retry. A failure recorded
// nothing, so the same photo must be readable again; the hash stays on
// the row because the fetched bytes are still the recovery anchor.
func readFailed(m *repository.InboundMessage) bool {
	return m.OutcomeReaction != nil && *m.OutcomeReaction == reactRejected
}

// captionRead is what a caption told us: the Source and category it
// named, the router's answer when the rules needed help, and — when
// nothing could read it — the note to quote back to the user.
type captionRead struct {
	Source     *entity.Source
	CategoryID *uuid.UUID
	Answer     *llm.AnswerFields
	Unread     string
}

// trimClause drops the punctuation that glues a clause to the rest of a
// caption ("date, dari BRI" leaves "date,"): the same rule parse applies
// before a clause is matched against a name.
func trimClause(s string) string {
	return strings.Trim(s, ",.;:!?\"'")
}

// explainedRemainder drops the politeness words a caption may carry
// around a clause ("semuanya kategori date"): a remainder that is only
// filler is explained, exactly as bareConfirm treats it for a reply.
func explainedRemainder(rest string) string {
	var kept []string
	for _, word := range strings.Fields(rest) {
		if !answerFiller[word] {
			kept = append(kept, word)
		}
	}
	return strings.Join(kept, " ")
}

// readCaption reads the words the user sent with the picture. The rules
// get first refusal — a Source or category clause they can resolve costs
// no model call, which is what makes a caption readable on a slow
// provider — and only a remainder they cannot explain is worth one
// router call, which can also carry a split payment. Whatever it returns
// is a hint — the caption can never confirm the draft, so Confirm,
// Merchant and OccurredDate are deliberately ignored here. A caption
// nothing could read comes back as Unread: the reply names the note
// instead of dropping it silently.
func (h *Harness) readCaption(
	ctx context.Context,
	caption string,
	params *receiptParams,
	sources []entity.Source,
	cats []entity.Category,
	lang string,
) captionRead {
	var out captionRead
	if caption == "" {
		return out
	}
	if hit := resolveSourceRules(caption, sources); hit != nil {
		out.Source = hit
		return out
	}
	rest := caption
	if hint, without := parse.SourceHint(caption); hint != "" {
		// A Source clause the rules cannot resolve is not consumed: the
		// clause stays in the remainder so the reply can name it.
		if hit := resolveSourceRules(hint, sources); hit != nil {
			out.Source = hit
			rest = without
		}
	}
	if hint, without := parse.CategoryHint(rest); hint != "" {
		if hit := resolveCategoryRules(hint, cats); hit != nil {
			out.CategoryID = &hit.ID
			rest = without
		}
	} else if hit := resolveCategoryRules(trimClause(rest), cats); hit != nil {
		// Without the keyword the whole remainder is the candidate, so a
		// bare "date" labels the receipt; the resolve gate keeps an
		// unreadable caption from consuming itself here.
		out.CategoryID = &hit.ID
		rest = ""
	}
	if explainedRemainder(trimClause(rest)) == "" {
		return out
	}
	if !h.llm.Enabled(ctx) {
		out.Unread = caption
		return out
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
		out.Unread = caption
		return out
	}
	if in.Answer == nil {
		out.Unread = caption
		return out
	}
	// The rules win: the router only fills what they left open.
	if out.Source == nil && in.Answer.SourceID != nil {
		out.Source = findSource(*in.Answer.SourceID, sources)
	}
	if out.CategoryID == nil && in.Answer.CategoryID != nil {
		out.CategoryID = in.Answer.CategoryID
	}
	out.Answer = in.Answer
	return out
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
		"Unread":    p.Unread,
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
