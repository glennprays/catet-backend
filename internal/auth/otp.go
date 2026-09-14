package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"time"

	"github.com/glennprays/catet-backend/internal/apperror"
	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/pkg/logctx"
	"github.com/glennprays/log"
	"github.com/google/uuid"
)

// OtpSender delivers a login code to the owner. The WhatsApp gateway's
// upstream HTTP shape is unverified, so it sits behind this interface
// — only HttpGatewaySender changes when the real contract lands.
type OtpSender interface {
	Send(ctx context.Context, phone string, code string) error
}

// LogSender prints the code to the API log. Default whenever
// WA_GATEWAY_URL is unset (development).
type LogSender struct {
	logger *log.Logger
}

func NewLogSender(logger *log.Logger) *LogSender {
	return &LogSender{logger: logger}
}

func (s *LogSender) Send(ctx context.Context, phone string, code string) error {
	s.logger.Info(logctx.TraceID(ctx), fmt.Sprintf("OTP untuk %s: %s", phone, code), nil)
	return nil
}

// HttpGatewaySender POSTs {phone, message} with a bearer token to the
// configured WhatsApp gateway URL.
type HttpGatewaySender struct {
	url   string
	token string
	http  *http.Client
}

func NewHttpGatewaySender(url, token string) *HttpGatewaySender {
	return &HttpGatewaySender{
		url:   url,
		token: token,
		http:  &http.Client{Timeout: 10 * time.Second},
	}
}

func (s *HttpGatewaySender) Send(ctx context.Context, phone string, code string) error {
	body, err := json.Marshal(map[string]string{
		"phone":   phone,
		"message": fmt.Sprintf("Kode catet kamu: %s (berlaku 5 menit).", code),
	})
	if err != nil {
		return fmt.Errorf("encode gateway payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build gateway request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("call whatsapp gateway: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("whatsapp gateway returned status %d", resp.StatusCode)
	}
	return nil
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

// OtpService implements request/verify with fail-closed attempt and
// expiry rules.
type OtpService struct {
	store      CodeStore
	sender     OtpSender
	tokens     *TokenService
	ownerPhone string
	ttl        time.Duration
	now        func() time.Time
	randomCode func() (string, error)
}

func NewOtpService(store CodeStore, sender OtpSender, tokens *TokenService, ownerPhone string, ttl time.Duration) *OtpService {
	return &OtpService{
		store:      store,
		sender:     sender,
		tokens:     tokens,
		ownerPhone: ownerPhone,
		ttl:        ttl,
		now:        time.Now,
		randomCode: generateCode,
	}
}

// Request validates the phone, rate-limits resends, then creates and
// delivers a fresh code.
func (s *OtpService) Request(ctx context.Context, phone string) error {
	if phone != s.ownerPhone {
		return apperror.BadRequest("phone is not registered")
	}

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

// Verify checks the code against the latest outstanding one and, on
// success, issues a session token.
func (s *OtpService) Verify(ctx context.Context, phone, code string) (string, time.Time, error) {
	now := s.now()
	latest, err := s.store.Latest(ctx, phone)
	if err != nil {
		return "", time.Time{}, apperror.Internal("failed to load otp codes").Wrap(err)
	}
	if latest == nil || latest.Consumed {
		return "", time.Time{}, apperror.BadRequest("code is invalid or expired")
	}
	if now.After(latest.ExpiresAt) {
		return "", time.Time{}, apperror.BadRequest("code is invalid or expired")
	}
	if latest.Attempts >= maxAttempts {
		// Defensive: the counter path below already consumes at 5,
		// but a stale row that slipped through stays fail-closed.
		if err := s.store.Consume(ctx, latest.ID); err != nil {
			return "", time.Time{}, apperror.Internal("failed to invalidate otp code").Wrap(err)
		}
		return "", time.Time{}, apperror.BadRequest("attempts_exceeded")
	}

	if HashCode(code) != latest.CodeHash {
		attempts, err := s.store.IncrementAttempts(ctx, latest.ID)
		if err != nil {
			return "", time.Time{}, apperror.Internal("failed to record attempt").Wrap(err)
		}
		if attempts >= maxAttempts {
			if err := s.store.Consume(ctx, latest.ID); err != nil {
				return "", time.Time{}, apperror.Internal("failed to invalidate otp code").Wrap(err)
			}
			return "", time.Time{}, apperror.BadRequest("attempts_exceeded")
		}
		return "", time.Time{}, apperror.BadRequest("code is invalid or expired")
	}

	if err := s.store.Consume(ctx, latest.ID); err != nil {
		return "", time.Time{}, apperror.Internal("failed to consume otp code").Wrap(err)
	}
	token, expiresAt, err := s.tokens.Issue(s.ownerPhone)
	if err != nil {
		return "", time.Time{}, apperror.Internal("failed to issue session").Wrap(err)
	}
	return token, expiresAt, nil
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
