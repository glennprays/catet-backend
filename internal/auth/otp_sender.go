package auth

import (
	"context"
	"fmt"

	"github.com/glennprays/catet-backend/internal/gateway"
	waga "github.com/glennprays/whatsapp-gateway-sdk-go"
)

// GatewayOtpSender delivers login codes over the same WhatsApp gateway
// seam the bot replies through — one transport, one place to configure.
type GatewayOtpSender struct {
	gw gateway.Sender
}

func NewGatewayOtpSender(gw gateway.Sender) *GatewayOtpSender {
	return &GatewayOtpSender{gw: gw}
}

func (s *GatewayOtpSender) Send(ctx context.Context, phone string, code string) error {
	chat := waga.FormatMSISDN(gateway.DigitsOnly(phone))
	_, err := s.gw.SendText(ctx, chat, fmt.Sprintf("Kode catet kamu: %s (berlaku 5 menit).", code), "")
	return err
}
