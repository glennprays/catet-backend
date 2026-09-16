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
	// botOrigin is the provenance every write from this surface carries.
	botOrigin = "whatsapp_text"
	// photoOrigin is the provenance of a line read off a receipt photo.
	photoOrigin = "whatsapp_photo"
	// photoDuplicateWindow bounds how far back the same image bytes are
	// remembered.
	photoDuplicateWindow = 7 * 24 * time.Hour
)

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
	cfg   *config.Config
	log   *log.Logger
	gw    gateway.Sender
	llm   *llm.Client
	txs   *service.TransactionService
	over  *service.OverviewService
	ana   *service.AnalyticsService
	bills *service.BillService

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
		txs: txs, over: over, ana: ana, bills: bills,
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
)

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
			// Only the inbound family matters; delivery receipts and
			// session events would just be acked and dropped.
			if err := sender.RegisterWebhook(ctx, h.cfg.WaWebhookURL, h.cfg.WaWebhookSecret, []string{"message.incoming"}); err != nil {
				h.log.Error(traceOf(ctx), "register webhook failed", map[string]any{"error": err.Error()})
			} else {
				h.log.Info(traceOf(ctx), "webhook registered", map[string]any{"url": h.cfg.WaWebhookURL})
			}
		}
	}

	stale, err := h.inbound.ListProcessing(ctx)
	if err != nil {
		h.log.Error(traceOf(ctx), "replay scan failed", map[string]any{"error": err.Error()})
		return
	}
	for _, m := range stale {
		p := &waga.IncomingWebhookPayload{
			Event:     waga.IncomingEventMessageMessageIncoming,
			Chat:      m.Chat,
			From:      m.FromJID,
			MessageId: m.MessageID,
			Timestamp: m.OccurredAt.Unix(),
			Text:      m.Body,
			Type:      waga.IncomingMessageType(m.Kind),
		}
		// A photo replay needs its media back: the row stores the URL and
		// mime the gateway reported, and Body holds the caption.
		if m.MediaURL != nil {
			p.Media = &waga.IncomingMessageMediaInfo{
				Type:     waga.IncomingMessageType(m.Kind),
				Url:      *m.MediaURL,
				MimeType: derefOr(m.MediaMime, ""),
				Caption:  m.Body,
			}
			p.Text = ""
		}
		h.accept(ctx, p)
	}
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
	// The message id is the natural correlation key: it ties every log
	// line back to the inbound_messages row.
	ctx = logctx.WithTraceID(ctx, "wa:"+p.MessageId)
	occurred := time.Unix(p.Timestamp, 0).UTC()
	if p.Timestamp == 0 {
		occurred = time.Now().UTC()
	}
	msg := &repository.InboundMessage{
		MessageID:  p.MessageId,
		Chat:       p.Chat,
		FromJID:    p.From,
		OccurredAt: occurred,
		Body:       p.Text,
		Kind:       string(p.Type),
		Status:     "processing",
	}

	// One chat is one conversation: the reply to a photo and the answer
	// the user types while the vision call is still running must not race
	// each other. Holding the lock for the whole pipeline is what makes
	// "photo, then 'bayar pake BCA'" land in that order.
	mu := h.lockFor(p.Chat)
	mu.Lock()
	defer mu.Unlock()

	// Media metadata, never the bytes. The log line is the only visibility
	// into what the gateway actually sends: with its media download
	// disabled, Url is a raw mmg.whatsapp.net link whose body is still
	// E2E-encrypted and unreadable here.
	if p.Media != nil {
		mediaURL, mediaMime := p.Media.Url, p.Media.MimeType
		if mediaURL != "" {
			msg.MediaURL = &mediaURL
		}
		if mediaMime != "" {
			msg.MediaMime = &mediaMime
		}
		// The caption is the user's own hint about the photo; keep it
		// searchable in the same column a text message uses.
		if strings.TrimSpace(p.Text) == "" {
			msg.Body = p.Media.Caption
		}
		h.log.Info(traceOf(ctx), "inbound media", map[string]any{
			"type": string(p.Media.Type), "mime": mediaMime,
			"size": p.Media.Size, "url_host": hostOf(mediaURL),
			"caption_len": len(p.Media.Caption),
		})
	}

	// Sender gate: the bot answers registered, active accounts only.
	// Anything else is recorded and dropped without a reply — a stranger
	// must not learn that this number is a bot. The one exception is a
	// number that was invited and has not signed up yet: it gets its
	// register link resent, because that is the message it is waiting
	// for.
	user, err := h.resolveSender(ctx, p.From)
	if err != nil {
		return fmt.Errorf("resolve sender: %w", err)
	}
	if p.IsGroup || user == nil || user.DeactivatedAt != nil {
		msg.Status = "dropped"
		if _, _, err := h.inbound.InsertProcessing(ctx, msg); err != nil {
			return fmt.Errorf("record dropped message: %w", err)
		}
		droppedTotal.Add(1)

		reason := "not registered"
		switch {
		case p.IsGroup:
			reason = "group chat"
		case user != nil:
			reason = "deactivated"
		default:
			// A retried delivery re-issues the link; the template tells
			// the user to trust the newest one.
			sent, err := h.inviter.Reissue(ctx, gateway.UserPart(p.From))
			if err != nil {
				h.log.Warn(traceOf(ctx), "invite resend failed", map[string]any{"error": err.Error()})
			}
			if sent {
				reason = "invited"
			}
		}
		h.log.Warn(traceOf(ctx), "inbound dropped", map[string]any{"from": p.From, "chat": p.Chat, "reason": reason})
		return nil
	}

	// From here on every repository call reads its tenant from the
	// context: the pipeline below cannot touch another account's ledger
	// even by mistake.
	ctx = scope.WithUserID(ctx, user.ID)
	msg.UserID = &user.ID
	if user.WhatsAppLID == nil && p.AddressingMode != "lid" {
		h.learnLID(ctx, user)
	}

	inserted, existing, err := h.inbound.InsertProcessing(ctx, msg)
	if err != nil {
		return fmt.Errorf("claim message: %w", err)
	}
	if !inserted {
		if existing == nil {
			return nil
		}
		// A retried delivery: replay the exact reply we already sent. The
		// idempotency key lets the gateway collapse it instead of sending
		// the same text twice.
		if existing.Status == "replied" && existing.ReplyText != nil {
			h.send(ctx, existing.Chat, *existing.ReplyText, "reply:"+existing.MessageID)
		}
		return nil
	}

	// Seen, immediately: everything below can take seconds, and a photo
	// or an LLM call leaves the user staring at an unanswered message.
	h.react(ctx, p.Chat, p.MessageId, p.From, reactSeen)
	h.typing(ctx, p.Chat, true)
	defer h.typing(ctx, p.Chat, false)

	// A question the message opens outranks anything it wrote, so the
	// outcome emoji needs to know how many were open before it ran.
	openBefore, err := h.pending.CountOpen(ctx)
	if err != nil {
		h.log.Warn(traceOf(ctx), "count open actions failed", map[string]any{"error": err.Error()})
		openBefore = 0
	}

	reply, txIDs, lang, err := h.handle(ctx, p)
	handleErr := err
	if err != nil {
		errorsTotal.Add(1)
		h.log.Error(traceOf(ctx), "handle failed", map[string]any{"message_id": p.MessageId, "error": err.Error()})
		reply = tmpl.Render(lang, "error_generic", nil)
		txIDs = nil
	}
	if lang != "" && lang != "id" {
		if err := h.inbound.SetLang(ctx, p.MessageId, lang); err != nil {
			h.log.Warn(traceOf(ctx), "record language failed", map[string]any{"error": err.Error()})
		}
	}
	if reply != "" {
		h.send(ctx, p.Chat, reply, "reply:"+p.MessageId)
	}
	// A photo stamps its own outcome inside photo(); every other message
	// gets it here, once the reply is out.
	if p.Type != waga.IncomingMessageTypeImage {
		h.react(ctx, p.Chat, p.MessageId, p.From, h.outcomeReaction(ctx, handleErr, openBefore, txIDs))
	}
	var replyPtr *string
	if reply != "" {
		replyPtr = &reply
	}
	if err := h.inbound.Finish(ctx, p.MessageId, "replied", replyPtr, txIDs); err != nil {
		return fmt.Errorf("finish message: %w", err)
	}
	processedTotal.Add(1)
	return nil
}

// outcomeReaction names what the message did to the ledger. A question
// it left open outranks a write: something still needs the user. A
// message that only informed (a recap, a balance, an unknown) clears
// the 👀 instead of claiming an effect it never had.
func (h *Harness) outcomeReaction(ctx context.Context, handleErr error, openBefore int64, txIDs []uuid.UUID) string {
	if handleErr != nil {
		return reactRejected
	}
	openAfter, err := h.pending.CountOpen(ctx)
	if err != nil {
		h.log.Warn(traceOf(ctx), "count open actions failed", map[string]any{"error": err.Error()})
	} else if openAfter > openBefore {
		return reactParked
	}
	if len(txIDs) > 0 {
		return reactWritten
	}
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
		return h.capture(ctx, p, wrapDrafts(parsed.Drafts), state, lang)
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
func (h *Harness) send(ctx context.Context, chat, text, idempotencyKey string) {
	if _, err := h.gw.SendText(ctx, chat, text, idempotencyKey); err != nil {
		h.log.Error(traceOf(ctx), "reply send failed", map[string]any{"chat": chat, "error": err.Error()})
	}
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
