package auth

import (
	"time"

	"github.com/glennprays/catet-backend/config"
	"github.com/glennprays/catet-backend/internal/gateway"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/glennprays/catet-backend/internal/service"
	"github.com/glennprays/log"
	"github.com/gofiber/fiber/v2"
)

// Wire providers. Kept in this file so the injector stays declarative.

func ProvideTokenService(cfg *config.Config) *TokenService {
	return NewTokenService(cfg.AuthSecret)
}

// ProvideOtpSender delivers codes through the shared gateway seam; the
// seam itself decides between the real gateway and the dev log sender.
func ProvideOtpSender(gw gateway.Sender) OtpSender {
	return NewGatewayOtpSender(gw)
}

func ProvideOtpService(store CodeStore, sender OtpSender, cfg *config.Config) *OtpService {
	return NewOtpService(store, sender, time.Duration(cfg.OtpTTLSeconds)*time.Second)
}

func ProvideAuthService(
	users *repository.UserRepository,
	invites *repository.InviteRepository,
	resets *repository.PasswordResetRepository,
	otp *OtpService,
	tokens *TokenService,
	onboarding *service.OnboardingService,
	gw gateway.Sender,
) *AuthService {
	return NewAuthService(users, invites, resets, otp, tokens, onboarding, gw)
}

func ProvideInviteService(
	invites *repository.InviteRepository,
	users *repository.UserRepository,
	gw gateway.Sender,
	logger *log.Logger,
	cfg *config.Config,
) *InviteService {
	return NewInviteService(invites, users, gw, logger, cfg.DashboardURL)
}

// ProvideRequireAuth builds the Fiber middleware. It needs the user
// repository because every request re-reads the account behind the
// token — that is what makes deactivation and epoch bumps immediate.
func ProvideRequireAuth(tokens *TokenService, users *repository.UserRepository) fiber.Handler {
	return RequireAuth(tokens, users)
}
