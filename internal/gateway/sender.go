package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"expvar"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/glennprays/catet-backend/config"
	"github.com/glennprays/catet-backend/pkg/logctx"
	"github.com/glennprays/log"
	waga "github.com/glennprays/whatsapp-gateway-sdk-go"
)

// authErrorsTotal counts gateway rejections of our bearer token — the
// symptom of an expired phone-scoped JWT. Exposed via expvar so the
// operator can see it without a metrics stack.
var authErrorsTotal = expvar.NewInt("bot_gateway_auth_errors_total")

// AuthErrorsTotal reports the number of 401s the gateway returned.
func AuthErrorsTotal() int64 { return authErrorsTotal.Value() }

// Sender delivers one outbound WhatsApp message. Every reply path in
// the bot and the OTP sender funnel through this seam, so development
// never depends on a reachable gateway.
type Sender interface {
	// SendText delivers text to a chat JID. idempotencyKey may be "";
	// when set, the gateway replays its original response instead of
	// sending a duplicate.
	SendText(ctx context.Context, chat, text, idempotencyKey string) (messageID string, err error)
	// React puts one emoji on an inbound message; an empty emoji removes it.
	React(ctx context.Context, chat, messageID, sender, emoji string) error
	// SetTyping shows or clears the "typing…" indicator in a chat.
	SetTyping(ctx context.Context, chat string, typing bool) error
	// ContactLID resolves the opaque WhatsApp "@lid" identity of a phone
	// number; "" when the gateway has none.
	ContactLID(ctx context.Context, phone string) (string, error)
}

// WagaSender talks to the real gateway over its REST API.
type WagaSender struct {
	client  *waga.Client
	logger  *log.Logger
	baseURL string
	token   string
	http    *http.Client

	// authLogMu guards the rate limiter for the 401 log line: a stale
	// token would otherwise log on every single send.
	authLogMu sync.Mutex
	authLogAt time.Time
}

const authLogInterval = 10 * time.Minute

func NewWagaSender(baseURL, token string, logger *log.Logger) *WagaSender {
	return &WagaSender{
		client: waga.NewClient(
			waga.WithBaseURL(baseURL),
			waga.WithToken(token),
			waga.WithTimeout(10*time.Second),
		),
		logger:  logger,
		baseURL: strings.TrimSuffix(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 10 * time.Second},
	}
}

// RegisterWebhook upserts our subscription. The SDK cannot express the
// per-subscription `events` filter, and a subscription without one
// receives every gateway event (message.sent, session.*) — traffic the
// bot only acks and throws away — so this one call is hand-rolled.
func (s *WagaSender) RegisterWebhook(ctx context.Context, url, secret string, events []string) error {
	payload, err := json.Marshal(map[string]any{
		"url":         url,
		"hmac_secret": secret,
		"events":      events,
	})
	if err != nil {
		return fmt.Errorf("encode webhook payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+"/webhook", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.token)

	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("register webhook: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("register webhook: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func (s *WagaSender) SendText(ctx context.Context, chat, text, idempotencyKey string) (string, error) {
	opts := []waga.SendOption{waga.WithChat(chat)}
	if idempotencyKey != "" {
		opts = append(opts, waga.WithIdempotencyKey(idempotencyKey))
	}
	resp, err := s.client.SendText(ctx, "", text, opts...)
	if err != nil {
		if errors.Is(err, waga.ErrUnauthorized) {
			authErrorsTotal.Add(1)
			s.logAuthError(ctx)
		}
		return "", fmt.Errorf("gateway send: %w", err)
	}
	// Debug-only: the message body is personal, so it is visible while
	// developing (LOG_LEVEL=debug) and silent at info and above.
	if s.logger != nil {
		s.logger.Debug(traceOf(ctx), fmt.Sprintf("[wa→%s] %s", chat, text), nil)
	}
	if resp == nil {
		return "", nil
	}
	return resp.MessageId, nil
}

// React acknowledges an inbound message. The gateway needs the author's
// JID as well as the chat, because a reaction is addressed to the
// message it decorates, not to the conversation.
func (s *WagaSender) React(ctx context.Context, chat, messageID, sender, emoji string) error {
	if err := s.client.React(ctx, chat, messageID, emoji, sender); err != nil {
		if errors.Is(err, waga.ErrUnauthorized) {
			authErrorsTotal.Add(1)
			s.logAuthError(ctx)
		}
		return fmt.Errorf("gateway react: %w", err)
	}
	if s.logger != nil {
		s.logger.Debug(traceOf(ctx), fmt.Sprintf("[wa→%s] react %s %s", chat, emoji, messageID), nil)
	}
	return nil
}

// SetTyping tells the user we are working on their message; it is the
// only feedback available while a slow read (vision) runs.
func (s *WagaSender) SetTyping(ctx context.Context, chat string, typing bool) error {
	state := waga.PresencePaused
	onOff := "off"
	if typing {
		state = waga.PresenceComposing
		onOff = "on"
	}
	if err := s.client.SendChatPresence(ctx, chat, state); err != nil {
		if errors.Is(err, waga.ErrUnauthorized) {
			authErrorsTotal.Add(1)
			s.logAuthError(ctx)
		}
		return fmt.Errorf("gateway typing: %w", err)
	}
	if s.logger != nil {
		s.logger.Debug(traceOf(ctx), fmt.Sprintf("[wa→%s] typing %s", chat, onOff), nil)
	}
	return nil
}

// ContactLID asks the gateway which "@lid" identity belongs to a phone
// number, so an account can still be recognised once WhatsApp starts
// addressing it in lid mode. An unknown contact is not an error: the
// gateway simply has no lid for it yet, and "" tells the caller to
// keep matching on the phone.
func (s *WagaSender) ContactLID(ctx context.Context, phone string) (string, error) {
	resp, err := s.client.GetContactInfo(ctx, waga.FormatMSISDN(DigitsOnly(phone)))
	if err != nil {
		if errors.Is(err, waga.ErrUnauthorized) {
			authErrorsTotal.Add(1)
			s.logAuthError(ctx)
		}
		return "", fmt.Errorf("gateway contact info: %w", err)
	}
	if resp == nil {
		return "", nil
	}
	return UserPart(resp.LID), nil
}

func (s *WagaSender) logAuthError(ctx context.Context) {
	s.authLogMu.Lock()
	defer s.authLogMu.Unlock()
	now := time.Now()
	if now.Sub(s.authLogAt) < authLogInterval {
		return
	}
	s.authLogAt = now
	if s.logger != nil {
		s.logger.Error(traceOf(ctx), "gateway token rejected (expired?)", nil)
	}
}

// LogSender prints outbound messages to the API log. Selected whenever
// WA_GATEWAY_URL is unset, so the whole bot runs offline in dev.
type LogSender struct {
	logger *log.Logger
}

func NewLogSender(logger *log.Logger) *LogSender {
	return &LogSender{logger: logger}
}

func (s *LogSender) SendText(ctx context.Context, chat, text, _ string) (string, error) {
	s.logger.Info(traceOf(ctx), fmt.Sprintf("[wa→%s] %s", chat, text), nil)
	return "", nil
}

// React logs the reaction instead of sending it; the sender JID is
// irrelevant offline.
func (s *LogSender) React(ctx context.Context, chat, messageID, _, emoji string) error {
	s.logger.Info(traceOf(ctx), fmt.Sprintf("[wa→%s] react %s %s", chat, emoji, messageID), nil)
	return nil
}

// SetTyping logs the indicator toggle so dev traces show the same
// lifecycle the real gateway would drive.
func (s *LogSender) SetTyping(ctx context.Context, chat string, typing bool) error {
	onOff := "off"
	if typing {
		onOff = "on"
	}
	s.logger.Info(traceOf(ctx), fmt.Sprintf("[wa→%s] typing %s", chat, onOff), nil)
	return nil
}

// ContactLID has nothing to look up offline: there is no contact
// directory without a gateway, so every number stays phone-matched.
func (s *LogSender) ContactLID(context.Context, string) (string, error) {
	return "", nil
}

// Provide selects the real sender only when a gateway URL is configured.
func Provide(cfg *config.Config, logger *log.Logger) Sender {
	if cfg.WaGatewayURL == "" {
		return NewLogSender(logger)
	}
	return NewWagaSender(cfg.WaGatewayURL, cfg.WaGatewayToken, logger)
}

// traceOf never returns "": the logger rejects an empty trace id, and a
// gateway call can originate outside an HTTP request (bot replay).
func traceOf(ctx context.Context) string {
	if id := logctx.TraceID(ctx); id != "" {
		return id
	}
	return "gateway"
}
