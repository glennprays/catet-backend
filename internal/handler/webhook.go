package handler

import (
	"errors"

	"github.com/glennprays/catet-backend/internal/apperror"
	"github.com/glennprays/catet-backend/internal/bot"
	"github.com/glennprays/catet-backend/internal/httperror"
	waga "github.com/glennprays/whatsapp-gateway-sdk-go"
	"github.com/gofiber/fiber/v2"
)

// WebhookHandler receives gateway deliveries. This is the one publicly
// reachable route in the API: its authentication is the per-subscription
// HMAC signature, compared in constant time by the SDK. Nothing else is
// trusted from the payload.
type WebhookHandler struct {
	verifier *waga.WebhookVerifier
	bot      *bot.Harness
}

func NewWebhookHandler(verifier *waga.WebhookVerifier, harness *bot.Harness) *WebhookHandler {
	return &WebhookHandler{verifier: verifier, bot: harness}
}

// Incoming acknowledges within milliseconds and processes in the
// background: the gateway retries a slow delivery, and every retry is a
// duplicate the harness then has to dedupe.
func (h *WebhookHandler) Incoming(c *fiber.Ctx) error {
	ev, err := h.verifier.ParseWebhook(c.Body(), c.Get("X-Webhook-Signature"))
	if err != nil {
		if errors.Is(err, waga.ErrInvalidSignature) {
			return apperror.Unauthorized("bad signature")
		}
		// An unknown or malformed event is not our problem to retry:
		// acknowledge it so the gateway stops redelivering.
		return httperror.OK(c, fiber.Map{"ok": true})
	}
	if ev.Event == waga.WebhookEventMessageIncoming && ev.Incoming != nil {
		h.bot.Accept(ev.Incoming)
	}
	return httperror.OK(c, fiber.Map{"ok": true})
}
