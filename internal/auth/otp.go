package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
	"time"

	"github.com/glennprays/catet-backend/internal/apperror"
	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/google/uuid"
)

// OtpSender delivers a WhatsApp code. Implemented by GatewayOtpSender
// (otp_sender.go) on top of the shared WhatsApp gateway seam; unit
// tests substitute in-memory fakes.
type OtpSender interface {
	Send(ctx context.Context, phone string, code string) error
}

// CodeStore persists OTP codes. Implemented by repository.OtpRepository;
// in-memory fakes back the unit tests.
type CodeStore interface {
	Create(ctx context.Context, phone, codeHash string, expiresAt time.Time) error
	Latest(ctx context.Context, phone string) (*entity.OtpCode, error)
	IncrementAttempts(ctx context.Context, id uuid.UUID) (int, error)
	Consume(ctx context.Context, id uuid.UUID) error
}

const (
	maxAttempts    = 5
	resendCooldown = time.Minute
	maxCodeValue   = 1_000_000
)

// OtpService implements request/check with fail-closed attempt and
// expiry rules. It no longer logs anyone in: a code only proves that
// the person holds the number, which is what signup and password reset
// need.
type OtpService struct {
	store      CodeStore
	sender     OtpSender
	ttl        time.Duration
	now        func() time.Time
	randomCode func() (string, error)
}

func NewOtpService(store CodeStore, sender OtpSender, ttl time.Duration) *OtpService {
	return &OtpService{
		store:      store,
		sender:     sender,
		ttl:        ttl,
		now:        time.Now,
		randomCode: generateCode,
	}
}

// Request rate-limits resends, then creates and delivers a fresh code.
// Whether a number deserves one is the caller's decision — this is the
// delivery mechanism, not the policy.
func (s *OtpService) Request(ctx context.Context, phone string) error {
	now := s.now()
	latest, err := s.store.Latest(ctx, phone)
	if err != nil {
		return apperror.Internal("failed to load otp codes").Wrap(err)
	}
	if latest != nil &&
		!latest.Consumed &&
		now.Before(latest.ExpiresAt) &&
		now.Sub(latest.CreatedAt) < resendCooldown {
		return apperror.TooManyRequests("a code was just requested; wait a minute before retrying")
	}

	code, err := s.randomCode()
	if err != nil {
		return apperror.Internal("failed to generate otp code").Wrap(err)
	}
	if err := s.store.Create(ctx, phone, HashCode(code), now.Add(s.ttl)); err != nil {
		return apperror.Internal("failed to store otp code").Wrap(err)
	}
	if err := s.sender.Send(ctx, phone, code); err != nil {
		return apperror.Internal("failed to deliver otp code").Wrap(err)
	}
	return nil
}

// Check verifies a code against the latest outstanding one and consumes
// it on success. Proof of the number, nothing more: the session, if any,
// is issued by whoever asked.
func (s *OtpService) Check(ctx context.Context, phone, code string) error {
	now := s.now()
	latest, err := s.store.Latest(ctx, phone)
	if err != nil {
		return apperror.Internal("failed to load otp codes").Wrap(err)
	}
	if latest == nil || latest.Consumed {
		return apperror.BadRequest("code is invalid or expired")
	}
	if now.After(latest.ExpiresAt) {
		return apperror.BadRequest("code is invalid or expired")
	}
	if latest.Attempts >= maxAttempts {
		// Defensive: the counter path below already consumes at 5,
		// but a stale row that slipped through stays fail-closed.
		if err := s.store.Consume(ctx, latest.ID); err != nil {
			return apperror.Internal("failed to invalidate otp code").Wrap(err)
		}
		return apperror.BadRequest("attempts_exceeded")
	}

	if HashCode(code) != latest.CodeHash {
		attempts, err := s.store.IncrementAttempts(ctx, latest.ID)
		if err != nil {
			return apperror.Internal("failed to record attempt").Wrap(err)
		}
		if attempts >= maxAttempts {
			if err := s.store.Consume(ctx, latest.ID); err != nil {
				return apperror.Internal("failed to invalidate otp code").Wrap(err)
			}
			return apperror.BadRequest("attempts_exceeded")
		}
		return apperror.BadRequest("code is invalid or expired")
	}

	if err := s.store.Consume(ctx, latest.ID); err != nil {
		return apperror.Internal("failed to consume otp code").Wrap(err)
	}
	return nil
}

// HashCode hashes a code for storage/comparison — sha256 is enough
// for a 6-digit code with a 5-minute life and attempt limiting.
func HashCode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// generateCode returns a uniformly random zero-padded 6-digit code.
func generateCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(maxCodeValue))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}
