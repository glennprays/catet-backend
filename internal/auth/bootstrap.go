package auth

import (
	"context"
	"fmt"

	"github.com/glennprays/catet-backend/config"
	"github.com/glennprays/catet-backend/internal/gateway"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/glennprays/log"
)

// BootstrapPhone is the placeholder migration 0006 gives the first
// admin. It is not a phone number and can never be normalised into one,
// so it cannot collide with a real account and cannot be logged into —
// the account is unreachable until it is claimed.
const BootstrapPhone = "bootstrap"

// ClaimBootstrapAdmin turns the migration's placeholder admin into a
// real account, once, from OWNER_PHONE.
//
// This is the whole story of how a fresh install gets its first user:
// there is no signup, only invitation, so somebody has to exist before
// anybody can be invited. The operator sets OWNER_PHONE for exactly one
// boot; afterwards the account owns its identity and the variable is
// ignored. The claimed admin still has no password — they set one
// through "Lupa kata sandi", which proves they hold the number.
//
// An unclaimed admin with no OWNER_PHONE is fatal: booting would leave
// an install nobody can ever log into.
func ClaimBootstrapAdmin(
	ctx context.Context,
	users *repository.UserRepository,
	gw gateway.Sender,
	cfg *config.Config,
	logger *log.Logger,
) error {
	admin, err := users.GetByPhone(ctx, BootstrapPhone)
	if err != nil {
		return fmt.Errorf("look up bootstrap admin: %w", err)
	}
	if admin == nil {
		if cfg.OwnerPhone != "" {
			logger.Info("bootstrap", "OWNER_PHONE is set but the bootstrap admin is already claimed; remove it", nil)
		}
		return nil
	}

	if cfg.OwnerPhone == "" {
		return fmt.Errorf("bootstrap admin unclaimed: set OWNER_PHONE once to claim it")
	}
	phone, err := NormalizePhone(cfg.OwnerPhone)
	if err != nil {
		return fmt.Errorf("OWNER_PHONE is not a valid phone number: %q", cfg.OwnerPhone)
	}
	taken, err := users.GetByPhone(ctx, phone)
	if err != nil {
		return fmt.Errorf("look up owner phone: %w", err)
	}
	if taken != nil {
		return fmt.Errorf("OWNER_PHONE %s already belongs to another account", phone)
	}

	// The lid is what lets the bot recognise the admin when WhatsApp
	// addresses them opaquely. An explicit OWNER_LID wins; otherwise ask
	// the gateway, and accept not knowing — the harness learns it on the
	// first message.
	var lid *string
	if digits := gateway.DigitsOnly(cfg.OwnerLid); digits != "" {
		lid = &digits
	} else if resolved, err := gw.ContactLID(ctx, phone); err != nil {
		logger.Warn("bootstrap", "could not resolve the owner's whatsapp lid", map[string]any{"error": err.Error()})
	} else if resolved != "" {
		lid = &resolved
	}

	if err := users.SetPhone(ctx, admin.ID, phone, lid); err != nil {
		return fmt.Errorf("claim bootstrap admin: %w", err)
	}
	logger.Info("bootstrap", "bootstrap admin claimed", map[string]any{
		"phone": phone,
		"lid":   derefOr(lid, "none"),
	})
	return nil
}

// derefOr reads an optional string for a log line.
func derefOr(s *string, alt string) string {
	if s == nil {
		return alt
	}
	return *s
}
