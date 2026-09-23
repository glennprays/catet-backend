// Package bot is the WhatsApp capture harness: it turns one inbound
// message into ledger writes, Hold Queue drafts and exactly one reply.
//
// Division of labour (ADR-0003): the deterministic parser owns every
// money fact; the LLM guesses a category, repairs a Source alias and —
// for anything the rules refused — classifies the message's intent, and
// anything it returns must resolve against a whitelist. A plain capture
// never reaches the model; a correction, a balance question or a
// free-form message does. Amounts it reports are re-derived from the
// message text, so the bot still never invents an amount, a Source or a
// date. Whatever remains undecided becomes one clarifying question,
// then a Hold Queue row.
package bot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/glennprays/catet-backend/config"
	"github.com/glennprays/catet-backend/internal/apperror"
	"github.com/glennprays/catet-backend/internal/bot/parse"
	"github.com/glennprays/catet-backend/internal/money"
	"github.com/glennprays/catet-backend/internal/bot/tmpl"
	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/gateway"
	"github.com/glennprays/catet-backend/internal/llm"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/glennprays/catet-backend/internal/scope"
	"github.com/glennprays/catet-backend/internal/service"
	"github.com/glennprays/catet-backend/pkg/logctx"
	"github.com/glennprays/log"
	waga "github.com/glennprays/whatsapp-gateway-sdk-go"
	"github.com/google/uuid"
)

const (
	// categoryReviewThreshold is the confidence below which an LLM label
	// still lands in Needs-Review. Self-reported confidence is never the
	// gate for *writing* — only for whether a human should look.
	categoryReviewThreshold = 0.6
	// refundLookback bounds the search for a refund's target.
	refundLookback = 30 * 24 * time.Hour
	// processTimeout bounds one message's whole pipeline (LLM included).
	processTimeout = 60 * time.Second
	// photoProcessTimeout is the same bound for a photo: reading a
	// receipt is a vision call (up to 2×LLM_VISION_TIMEOUT_SECONDS) on
	// top of the media download.
	photoProcessTimeout = 150 * time.Second
	// finalizationTail reserves time after the core work deadline for
	// storing and delivering a durable result.
	textFinalizationTail = 15 * time.Second
	photoFinalizationTail = 20 * time.Second
	recoveryInterval = 30 * time.Second
	recoveryBatchSize = 100
	// botOrigin is the provenance every write from this surface carries.
	botOrigin = "whatsapp_text"
	// photoOrigin is the provenance of a line read off a receipt photo.
	photoOrigin = "whatsapp_photo"
	// photoDuplicateWindow bounds how far back the same image bytes are
	// remembered.
	photoDuplicateWindow = 7 * 24 * time.Hour
)
type handlingResult struct {
	Reply           string
	TransactionIDs  []uuid.UUID
	Lang            string
	OutcomeReaction string
	Stage           string
}

// inviter re-sends a register link to a number that was invited but has
// not signed up yet. Declared here rather than taken as a concrete type
// so the harness depends on the one behaviour it needs — and so the
// invite repository stays out of the bot.
type inviter interface {
	Reissue(ctx context.Context, phoneDigits string) (bool, error)
}

// Harness owns the inbound pipeline. No queue, no worker pool: each
// webhook is processed on its own goroutine, deduplicated by
// message_id, and serialised per chat. Every message is resolved to one
// account first; everything below then runs under that account's scope,
// so a capture can only ever touch its own ledger.
type Harness struct {
	cfg *config.Config
	log *log.Logger
	gw  gateway.Sender
	llm *llm.Client
	txs *service.TransactionService
	// merchants canonicalises whatever the chat or a receipt called the
	// shop, so memory lookups and the reply all speak one name.
	merchants *service.MerchantService
	over      *service.OverviewService
	ana       *service.AnalyticsService
	bills     *service.BillService

	sources    *repository.SourceRepository
	categories *repository.CategoryRepository
	txRepo     *repository.TransactionRepository
	pending    *repository.PendingActionRepository
	inbound    *repository.InboundMessageRepository
	chats      *repository.BotChatStateRepository
	analytics  *repository.AnalyticsRepository
	settings   *repository.SettingsRepository
	users      *repository.UserRepository
	inviter    inviter
	// billRepo and reminderSends resolve which bill a bare `lunas`
	// means: due today, overdue, or named in today's digest.
	billRepo      *repository.BillRepository
	reminderSends *repository.BillReminderSendRepository

	// http downloads inbound media. No client-level timeout: the deadline
	// rides on the context, like llm.New.
	http *http.Client

	// chatLocks serialises the pipeline per chat so a text sent seconds
	// after a photo cannot overtake the photo's slow vision call and find
	// no question to answer. One API instance only: this is a
	// process-local guarantee, not a distributed lock.
	chatLocks sync.Map

	// reactWarn keeps the "this gateway cannot react" notice to one line
	// per process: the failure repeats on every photo.
	reactWarn sync.Once

	// lidWarn keeps the "could not resolve this account's lid" notice to
	// one line per process: the lookup is retried on every message from
	// an account whose lid is still unknown.
	lidWarn sync.Once
}

func NewHarness(
	cfg *config.Config,
	logger *log.Logger,
	gw gateway.Sender,
	llmClient *llm.Client,
	txs *service.TransactionService,
	merchants *service.MerchantService,
	over *service.OverviewService,
	ana *service.AnalyticsService,
	bills *service.BillService,
	sources *repository.SourceRepository,
	categories *repository.CategoryRepository,
	txRepo *repository.TransactionRepository,
	pending *repository.PendingActionRepository,
	inbound *repository.InboundMessageRepository,
	chats *repository.BotChatStateRepository,
	analytics *repository.AnalyticsRepository,
	settings *repository.SettingsRepository,
	users *repository.UserRepository,
	billRepo *repository.BillRepository,
	reminderSends *repository.BillReminderSendRepository,
	inv inviter,
) *Harness {
	return &Harness{
		cfg: cfg, log: logger.With(log.String("component", "bot")), gw: gw, llm: llmClient,
		txs: txs, merchants: merchants, over: over, ana: ana, bills: bills,
		sources: sources, categories: categories, txRepo: txRepo,
		pending: pending, inbound: inbound, chats: chats, analytics: analytics,
		settings: settings, users: users, inviter: inv,
		billRepo: billRepo, reminderSends: reminderSends,
		http: &http.Client{},
	}
}

// lockFor is the chat's serialisation point. sync.Map is the right
// shape here: a handful of chats, each written once and read forever.
func (h *Harness) lockFor(chat string) *sync.Mutex {
	mu, _ := h.chatLocks.LoadOrStore(chat, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// The reaction alphabet. Every accepted message is stamped the moment
// it arrives and again when its outcome is known: seen, parked,
// written, refused. They are the only emoji this codebase produces — a
// reaction is not copy, it is the message's state. The empty string
// removes the mark, which is how an informational answer (a recap, a
// balance) says "nothing happened to your money".
const (
	reactSeen     = "👀"
	reactParked   = "📋"
	reactWritten  = "✅"
	reactRejected = "❌"
	reactNone     = ""
	// A persisted result must distinguish "clear the 👀" from an old row
	// with no outcome at all; the gateway receives the empty string again.
	reactionClearMarker = "\u200b"
)

func persistedReaction(reaction string) string {
	if reaction == reactNone { return reactionClearMarker }
	return reaction
}

func outboundReaction(reaction *string) (string, bool) {
	if reaction == nil { return "", false }
	if *reaction == reactionClearMarker { return reactNone, true }
	return *reaction, true
}

// react puts one emoji on the user's own message: the photo pipeline's
// only way to say "seen" before the vision call returns. Delivery is
// best-effort — a gateway that refuses reactions must not fail a
// capture.
func (h *Harness) react(ctx context.Context, chat, messageID, from, emoji string) {
	if chat == "" || messageID == "" {
		return
	}
	if err := h.gw.React(ctx, chat, messageID, from, emoji); err != nil {
		h.reactWarn.Do(func() {
			h.log.Warn(traceOf(ctx), "reaction unsupported", map[string]any{"error": err.Error()})
		})
	}
}

// typing shows or clears the "typing…" indicator. Same best-effort
// contract as react: it is an acknowledgement, not a result.
func (h *Harness) typing(ctx context.Context, chat string, on bool) {
	if chat == "" {
		return
	}
	if err := h.gw.SetTyping(ctx, chat, on); err != nil {
		h.log.Debug(traceOf(ctx), "typing indicator failed", map[string]any{"error": err.Error()})
	}
}

// Start registers the webhook subscription (when a public URL is
// configured) and replays messages a crash left mid-flight. Neither
// failure is fatal: the API must boot without the gateway.
func (h *Harness) Start(ctx context.Context) {
	if h.cfg.WaWebhookURL != "" {
		if sender, ok := h.gw.(*gateway.WagaSender); ok {
			if err := sender.RegisterWebhook(ctx, h.cfg.WaWebhookURL, h.cfg.WaWebhookSecret, []string{"message.incoming"}); err != nil {
				h.log.Error(traceOf(ctx), "register webhook failed", map[string]any{"error": err.Error()})
			} else {
				h.log.Info(traceOf(ctx), "webhook registered", map[string]any{"url": h.cfg.WaWebhookURL})
			}
		}
	}
	lifetime := context.WithoutCancel(ctx)
	recoverDue := func() {
		scanCtx, cancel := context.WithTimeout(lifetime, 10*time.Second)
		defer cancel()
		rows, err := h.inbound.ListDueInbound(scanCtx, recoveryBatchSize)
		if err != nil {
			h.log.Error(traceOf(scanCtx), "replay scan failed", map[string]any{"error": err.Error()})
			return
		}
		for i := range rows { h.accept(lifetime, inboundPayload(&rows[i])) }
	}
	recoverDue()
	go func() {
		ticker := time.NewTicker(recoveryInterval)
		defer ticker.Stop()
		for range ticker.C { recoverDue() }
	}()
}

func inboundPayload(m *repository.InboundMessage) *waga.IncomingWebhookPayload {
	p := &waga.IncomingWebhookPayload{
		Event: waga.IncomingEventMessageMessageIncoming, Chat: m.Chat, From: m.FromJID,
		MessageId: m.MessageID, Timestamp: m.OccurredAt.Unix(), Text: m.Body,
		Type: waga.IncomingMessageType(m.Kind),
	}
	if m.MediaURL != nil {
		p.Media = &waga.IncomingMessageMediaInfo{
			Type: waga.IncomingMessageType(m.Kind), Url: *m.MediaURL,
			MimeType: derefOr(m.MediaMime, ""), Caption: m.Body,
		}
		p.Text = ""
	}
	return p
}

// Accept hands the payload to a background goroutine so the webhook
// route can acknowledge within milliseconds; the gateway retries on a
// slow ack and every retry would be a duplicate.
func (h *Harness) Accept(p *waga.IncomingWebhookPayload) {
	h.accept(context.Background(), p)
}

// accept detaches from the caller's cancellation (the HTTP request is
// already answered, the boot context is about to be cancelled) while
// keeping its values, then bounds the work with its own timeout.
func (h *Harness) accept(ctx context.Context, p *waga.IncomingWebhookPayload) {
	base := context.WithoutCancel(ctx)
	timeout := processTimeout
	if p.Type == waga.IncomingMessageTypeImage {
		timeout = photoProcessTimeout
	}
	go func() {
		ctx, cancel := context.WithTimeout(base, timeout)
		defer cancel()
		if err := h.Process(ctx, p); err != nil {
			errorsTotal.Add(1)
			h.log.Error(traceOf(ctx), "process inbound failed", map[string]any{
				"message_id": p.MessageId, "error": err.Error(),
			})
		}
	}()
}

// traceOf keeps log calls correlated; the logger refuses an empty id, so
// a context without one falls back to a fixed component tag.
func traceOf(ctx context.Context) string {
	if id := logctx.TraceID(ctx); id != "" {
		return id
	}
	return "bot"
}

// resolveSender maps an inbound JID to the account it belongs to. The
// gateway addresses the same person either by phone digits or by an
// opaque lid, so both columns are candidates; nil means nobody here
// owns this number.
func (h *Harness) resolveSender(ctx context.Context, from string) (*entity.User, error) {
	digits := gateway.UserPart(from)
	if digits == "" {
		return nil, nil
	}
	return h.users.FindByWhatsApp(ctx, digits)
}

// learnLID fills in an account's opaque WhatsApp identity the first
// time we see it, so a later lid-addressed message still resolves.
// Best-effort: a gateway that cannot tell us must not fail a capture.
func (h *Harness) learnLID(ctx context.Context, user *entity.User) {
	lid, err := h.gw.ContactLID(ctx, user.Phone)
	if err != nil {
		h.lidWarn.Do(func() {
			h.log.Warn(traceOf(ctx), "whatsapp lid lookup failed", map[string]any{"error": err.Error()})
		})
		return
	}
	if lid == "" {
		return
	}
	if err := h.users.SetLID(ctx, user.ID, lid); err != nil {
		h.log.Warn(traceOf(ctx), "store whatsapp lid failed", map[string]any{"error": err.Error()})
	}
}

// Process runs the whole pipeline for one message. Exported so the boot
// replay and the integration tests can run it synchronously.
func (h *Harness) Process(ctx context.Context, p *waga.IncomingWebhookPayload) error {
	ctx = logctx.WithTraceID(ctx, "wa:"+p.MessageId)
	occurred := time.Unix(p.Timestamp, 0).UTC()
	if p.Timestamp == 0 { occurred = time.Now().UTC() }
	msg := &repository.InboundMessage{
		MessageID: p.MessageId, Chat: p.Chat, FromJID: p.From, OccurredAt: occurred,
		Body: p.Text, Kind: string(p.Type), Status: "processing",
	}
	if p.Media != nil {
		u, mime := p.Media.Url, p.Media.MimeType
		if u != "" { msg.MediaURL = &u }; if mime != "" { msg.MediaMime = &mime }
		if strings.TrimSpace(p.Text) == "" { msg.Body = p.Media.Caption }
		h.log.Info(traceOf(ctx), "inbound media", map[string]any{"type": string(p.Media.Type), "mime": mime, "size": p.Media.Size, "url_host": hostOf(u), "caption_len": len(p.Media.Caption)})
	}
	mu := h.lockFor(p.Chat); mu.Lock(); defer mu.Unlock()
	user, err := h.resolveSender(ctx, p.From)
	if err != nil { return fmt.Errorf("resolve sender: %w", err) }
	if user != nil {
		ctx = scope.WithUserID(ctx, user.ID)
		msg.UserID = &user.ID
		if user.WhatsAppLID == nil && p.AddressingMode != "lid" { h.learnLID(ctx, user) }
	}
	var claim *repository.InboundClaim
	gated := p.IsGroup || user == nil || user.DeactivatedAt != nil
	if gated {
		msg.Status = "dropped"
		claim, err = h.inbound.ClaimInbound(ctx, msg)
		if err != nil { return fmt.Errorf("record dropped message: %w", err) }
		if claim.Kind == repository.InboundClaimReplay || claim.Kind == repository.InboundClaimBusy || user == nil {
			if user == nil && !p.IsGroup && h.inviter != nil { _, _ = h.inviter.Reissue(ctx, gateway.UserPart(p.From)) }
			return nil
		}
	} else {
		claim, err = h.inbound.ClaimInbound(ctx, msg)
		if err != nil { return fmt.Errorf("claim message: %w", err) }
	}
	if gated && claim.Kind != repository.InboundClaimWork && claim.Kind != repository.InboundClaimDelivery && claim.Kind != repository.InboundClaimIncomplete { return nil }
	if !gated && claim.Kind == repository.InboundClaimBusy { return nil }
	if !gated && claim.Kind == repository.InboundClaimReplay {
		if claim.Message != nil && claim.Message.Status == "replied" && claim.Message.ReplyText != nil {
			fctx, cancel := h.finalizeContext(ctx); defer cancel()
			if err := h.send(fctx, claim.Message.Chat, *claim.Message.ReplyText, "reply:"+claim.Message.MessageID); err != nil { h.log.Warn(traceOf(fctx), "replay send failed", map[string]any{"error": err.Error()}) }
		}
		return nil
	}
	if claim.Kind == repository.InboundClaimDelivery { return h.deliverClaim(ctx, claim) }
	if claim.Kind == repository.InboundClaimIncomplete {
		fctx, cancel := h.finalizeContext(ctx); defer cancel()
		return h.finalizeIncomplete(fctx, claim)
	}
	if claim.Kind != repository.InboundClaimWork { return nil }
	h.react(ctx, p.Chat, p.MessageId, p.From, reactSeen)
	h.typing(ctx, p.Chat, true)
	defer func() { fctx, cancel := h.finalizeContext(ctx); defer cancel(); h.typing(fctx, p.Chat, false) }()
	openBefore, err := h.pending.CountOpen(ctx); if err != nil { openBefore = 0 }
	workCtx, cancel := reserveWorkContext(ctx, p.Type == waga.IncomingMessageTypeImage); defer cancel()
	result, handleErr, panicked := h.runHandle(workCtx, p)
	if result.Lang == "" { result.Lang = "id" }
	if handleErr != nil {
		errorsTotal.Add(1)
		h.log.Error(traceOf(ctx), "handle failed", map[string]any{"message_id": p.MessageId, "error": handleErr.Error()})
		result.Reply = tmpl.Render(result.Lang, "error_generic", nil)
		result.OutcomeReaction = reactRejected
	}
	if result.OutcomeReaction == "" {
		result.OutcomeReaction = h.outcomeReaction(ctx, handleErr, openBefore, result.TransactionIDs, p.Type == waga.IncomingMessageTypeImage)
	}
	if panicked {
		fctx, fcancel := h.finalizeContext(ctx); defer fcancel()
		return h.handlePanic(fctx, claim.Lease, result, p)
	}
	fctx, fcancel := h.finalizeContext(ctx); defer fcancel()
	delivery, err := h.inbound.StoreInboundResult(fctx, claim.Lease, repository.InboundResult{
		ReplyText: result.Reply, TransactionIDs: result.TransactionIDs, Lang: result.Lang, OutcomeReaction: persistedReaction(result.OutcomeReaction),
	})
	if err != nil { return h.releaseWork(fctx, claim.Lease, result.TransactionIDs, result.Lang, "result_store_failed", p.Chat, p.MessageId, p.From) }
	if err := h.send(fctx, p.Chat, result.Reply, "reply:"+p.MessageId); err != nil {
		_ = h.inbound.RescheduleInbound(fctx, delivery, "reply_transport_timeout")
		return nil
	}
	if err := h.inbound.MarkInboundReplied(fctx, delivery); err != nil { return fmt.Errorf("mark replied: %w", err) }
	h.react(fctx, p.Chat, p.MessageId, p.From, result.OutcomeReaction)
	processedTotal.Add(1)
	return nil
}

func reserveWorkContext(ctx context.Context, photo bool) (context.Context, context.CancelFunc) {
	tail := textFinalizationTail; if photo { tail = photoFinalizationTail }
	if deadline, ok := ctx.Deadline(); ok { return context.WithDeadline(ctx, deadline.Add(-tail)) }
	return context.WithCancel(ctx)
}

func (h *Harness) finalizeContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
}
func (h *Harness) runHandle(ctx context.Context, p *waga.IncomingWebhookPayload) (result handlingResult, err error, panicked bool) {
	result.Stage = "text"
	if p.Type == waga.IncomingMessageTypeImage { result.Stage = "photo" }
	defer func() {
		if v := recover(); v != nil {
			panicked = true
			err = fmt.Errorf("panic: %v", v)
		}
	}()
	if p.Type == waga.IncomingMessageTypeImage && p.Media != nil {
		result.Lang = parse.Parse(p.Media.Caption, service.JakartaNow()).Lang
		if result.Lang == "" { result.Lang = "id" }
		result.Reply, result.TransactionIDs, result.OutcomeReaction, err = h.readPhoto(ctx, p, result.Lang)
		return
	}
	result.Reply, result.TransactionIDs, result.Lang, err = h.handle(ctx, p)
	return
}

func (h *Harness) deliverClaim(ctx context.Context, claim *repository.InboundClaim) error {
	if claim.Message == nil || claim.Message.ReplyText == nil { return nil }
	fctx, cancel := h.finalizeContext(ctx); defer cancel()
	if err := h.send(fctx, claim.Message.Chat, *claim.Message.ReplyText, "reply:"+claim.Message.MessageID); err != nil {
		_ = h.inbound.RescheduleInbound(fctx, claim.Lease, "reply_transport_timeout"); return nil
	}
	if err := h.inbound.MarkInboundReplied(fctx, claim.Lease); err != nil { return err }
	reaction, ok := outboundReaction(claim.Message.OutcomeReaction)
	if ok { h.react(fctx, claim.Message.Chat, claim.Message.MessageID, claim.Message.FromJID, reaction) }
	return nil
}

func (h *Harness) releaseWork(ctx context.Context, lease *repository.InboundLease, ids []uuid.UUID, lang, stage, chat, messageID, from string) error {
	final, err := h.inbound.ReleaseInboundWork(ctx, lease, ids, stage)
	if err != nil { return err }
	if final { return h.storeIncomplete(ctx, lease, ids, lang, chat, messageID, from) }
	return nil
}

func (h *Harness) handlePanic(ctx context.Context, lease *repository.InboundLease, result handlingResult, p *waga.IncomingWebhookPayload) error {
	stage := result.Stage
	if stage == "" { stage = "inbound_work" }
	h.log.Error(traceOf(ctx), "inbound work panic", map[string]any{"stage": stage})
	return h.releaseWork(ctx, lease, result.TransactionIDs, result.Lang, stage+"_panic", p.Chat, p.MessageId, p.From)
}

func (h *Harness) finalizeIncomplete(ctx context.Context, claim *repository.InboundClaim) error {
	if claim.Message == nil { return nil }
	return h.storeIncomplete(ctx, claim.Lease, claim.Message.TransactionIDs, claim.Message.Lang, claim.Message.Chat, claim.Message.MessageID, claim.Message.FromJID)
}

func (h *Harness) storeIncomplete(ctx context.Context, lease *repository.InboundLease, ids []uuid.UUID, lang, chat, messageID, from string) error {
	if lang == "" { lang = "id" }
	items := make([]writtenItem, 0, len(ids))
	for i, id := range ids {
		tx, err := h.txRepo.Get(ctx, id); if err != nil || tx == nil { continue }
		merchant := ""; if tx.Merchant != nil { merchant = *tx.Merchant }
		items = append(items, writtenItem{N: i + 1, Amount: money.FormatIDR(tx.AmountMinor), Merchant: merchant})
	}
	reply := tmpl.Render(lang, "capture_incomplete", map[string]any{"Items": items})
	delivery, err := h.inbound.StoreInboundResult(ctx, lease, repository.InboundResult{ReplyText: reply, TransactionIDs: ids, Lang: lang, OutcomeReaction: reactRejected})
	if err != nil { return err }
	if err := h.send(ctx, chat, reply, "reply:"+messageID); err != nil { _ = h.inbound.RescheduleInbound(ctx, delivery, "reply_transport_timeout"); return nil }
	if err := h.inbound.MarkInboundReplied(ctx, delivery); err != nil { return err }
	h.react(ctx, chat, messageID, from, reactRejected)
	return nil
}
// outcomeReaction names what the message did to the ledger. A question
// it left open outranks a write: something still needs the user. A
// message that only informed (a recap, a balance, an unknown) clears
// the 👀 instead of claiming an effect it never had.
func (h *Harness) outcomeReaction(ctx context.Context, handleErr error, openBefore int64, txIDs []uuid.UUID, image bool) string {
	if handleErr != nil { return reactRejected }
	openAfter, err := h.pending.CountOpen(ctx)
	if err != nil { h.log.Warn(traceOf(ctx), "count open actions failed", map[string]any{"error": err.Error()}) } else if openAfter > openBefore { return reactParked }
	if len(txIDs) > 0 { return reactWritten }
	if image { return reactRejected }
	return reactNone
}

// handle produces the reply text and the transactions the message wrote.
func (h *Harness) handle(ctx context.Context, p *waga.IncomingWebhookPayload) (string, []uuid.UUID, string, error) {
	// A photo is its own capture surface: the caption, not the body, is
	// the user's hint, and there is no money to parse out of text.
	if p.Type == waga.IncomingMessageTypeImage && p.Media != nil {
		lang := parse.Parse(p.Media.Caption, service.JakartaNow()).Lang
		if lang == "" {
			lang = "id"
		}
		return h.photo(ctx, p, lang)
	}
	if p.Type != waga.IncomingMessageTypeText || strings.TrimSpace(p.Text) == "" {
		return tmpl.Render("id", "unsupported_media", nil), nil, "id", nil
	}

	parsed := parse.Parse(p.Text, service.JakartaNow())
	lang := parsed.Lang
	if lang == "" {
		lang = "id"
	}

	state, err := h.chats.Get(ctx, p.Chat)
	if err != nil {
		return "", nil, lang, fmt.Errorf("load chat state: %w", err)
	}
	if state == nil {
		state = &repository.ChatState{Chat: p.Chat}
	}

	// An outstanding question owns the next message: answering it is the
	// only way a Hold turns into a write without the dashboard. The rules
	// get first refusal — a message they can explain word for word costs
	// no model call — and a message that settles nothing leaves the
	// question standing for the intent stage below.
	if state.OpenPendingActionID != nil {
		sources, err := h.sources.List(ctx, false)
		if err != nil {
			return "", nil, lang, fmt.Errorf("list sources: %w", err)
		}
		expect := ""
		if state.OpenExpect != nil {
			expect = *state.OpenExpect
		}
		reply, ids, handled, err := h.applyAnswer(
			ctx, parseBareAnswer(p.Text, expect, sources), p.MessageId, state, lang)
		if err != nil {
			return "", nil, lang, err
		}
		if handled {
			return reply, ids, lang, nil
		}
	} else if parsed.Command == "ya" || parsed.Command == "tidak" {
		return tmpl.Render(lang, "no_open_question", nil), nil, lang, nil
	}

	switch parsed.Command {
	case "batal":
		reply, ids, err := h.batal(ctx, state, parsed.BatalIndex, lang)
		return reply, ids, lang, err
	case "rekap_hari":
		reply, err := h.rekapHari(ctx, lang)
		return reply, nil, lang, err
	case "rekap_minggu":
		reply, err := h.rekapMinggu(ctx, lang)
		return reply, nil, lang, err
	case "lunas":
		reply, ids, err := h.lunas(ctx, p.Chat, p.MessageId, state, parsed.BillQuery, parsed.Amount, lang)
		if err != nil {
			return "", nil, lang, err
		}
		if err := h.saveState(ctx, state); err != nil {
			return "", nil, lang, err
		}
		return reply, ids, lang, nil
	}

	// Fast path: a plain capture the rules fully decided. Everything the
	// rules refused — a hold, wording that points at an earlier
	// transaction, no draft at all, or an unanswered question — goes to
	// the intent stage instead of being forced into a write.
	if state.OpenPendingActionID == nil && len(parsed.Drafts) > 0 &&
		!anyHold(parsed.Drafts) && !parse.LooksReferential(p.Text) {
		return h.capture(ctx, p, wrapDrafts(parsed.Drafts), state, lang, parsed.SourceFooter)
	}
	return h.intentStage(ctx, p, parsed, state, lang)
}

// anyHold reports whether the parser refused to decide any draft.
func anyHold(drafts []parse.Draft) bool {
	for i := range drafts {
		if drafts[i].Hold != nil {
			return true
		}
	}
	return false
}

// send delivers one reply; a delivery failure never rolls back a write.
func (h *Harness) send(ctx context.Context, chat, text, idempotencyKey string) error {
	if _, err := h.gw.SendText(ctx, chat, text, idempotencyKey); err != nil {
		h.log.Error(traceOf(ctx), "reply send failed", map[string]any{"chat": chat, "error": err.Error()})
		return err
	}
	return nil

}
// saveState persists the chat's batch pointer and open question.
func (h *Harness) saveState(ctx context.Context, state *repository.ChatState) error {
	if err := h.chats.Upsert(ctx, state); err != nil {
		return fmt.Errorf("save chat state: %w", err)
	}
	return nil
}

// contentHash is the capture's dedup fingerprint: same text, same
// Jakarta day.
func contentHash(text string, day string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(text)) + "|" + day))
	return hex.EncodeToString(sum[:])
}

// hostOf is the log-safe part of a media URL: enough to tell the
// gateway's own storage from WhatsApp's CDN without logging a token.
func hostOf(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "invalid"
	}
	return u.Host
}

// derefOr reads an optional string column.
func derefOr(s *string, alt string) string {
	if s == nil {
		return alt
	}
	return *s
}

// errIsConflict reports whether err is a ledger conflict: already
// written (duplicate external_id) or already reversed.
func errIsConflict(err error) bool {
	var ae *apperror.Error
	return errors.As(err, &ae) && ae.Kind == apperror.KindConflict
}

// jsonParams marshals pending-action params, panicking only on
// programmer error (the structs are static).
func jsonParams(v any) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode params: %w", err)
	}
	return json.RawMessage(b), nil
}

// sourceNames lists the whitelisted Source names for a question.
func sourceNames(sources []entity.Source) []string {
	out := make([]string, 0, len(sources))
	for _, s := range sources {
		out = append(out, s.Name)
	}
	return out
}
