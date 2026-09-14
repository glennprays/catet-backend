package auth

import (
	"time"

	"github.com/glennprays/catet-backend/config"
	"github.com/glennprays/log"
	"github.com/gofiber/fiber/v2"
)

// Wire providers. Kept in this file so the injector stays declarative.

func ProvideTokenService(cfg *config.Config) *TokenService {
	return NewTokenService(cfg.AuthSecret)
}

// ProvideOtpSender selects the log sender whenever WA_GATEWAY_URL is
// unset — development never depends on the gateway being reachable.
func ProvideOtpSender(cfg *config.Config, logger *log.Logger) OtpSender {
	if cfg.WaGatewayURL == "" {
		return NewLogSender(logger)
	}
	return NewHttpGatewaySender(cfg.WaGatewayURL, cfg.WaGatewayToken)
}

func ProvideOtpService(store CodeStore, sender OtpSender, tokens *TokenService, cfg *config.Config) *OtpService {
	return NewOtpService(store, sender, tokens, cfg.OwnerPhone, time.Duration(cfg.OtpTTLSeconds)*time.Second)
}

// ProvideRequireAuth builds the Fiber middleware from the token service.
func ProvideRequireAuth(tokens *TokenService) fiber.Handler {
	return RequireAuth(tokens)
}
