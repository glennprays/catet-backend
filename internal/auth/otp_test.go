package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/glennprays/catet-backend/internal/apperror"
	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testPhone is any number at all: the OTP service no longer knows who
// deserves a code — eligibility is the caller's policy.
const testPhone = "+628000000000"

// fakeStore keeps a single outstanding code; the clock is injectable.
type fakeStore struct {
	now  time.Time
	code *entity.OtpCode
}

func (s *fakeStore) Create(_ context.Context, phone, codeHash string, expiresAt time.Time) error {
	s.code = &entity.OtpCode{
		ID:        uuid.New(),
		Phone:     phone,
		CodeHash:  codeHash,
		ExpiresAt: expiresAt,
		CreatedAt: s.now,
	}
	return nil
}

func (s *fakeStore) Latest(_ context.Context, _ string) (*entity.OtpCode, error) {
	return s.code, nil
}

func (s *fakeStore) IncrementAttempts(_ context.Context, _ uuid.UUID) (int, error) {
	s.code.Attempts++
	return s.code.Attempts, nil
}

func (s *fakeStore) Consume(_ context.Context, _ uuid.UUID) error {
	s.code.Consumed = true
	return nil
}

// fakeOtpSender is an auth.OtpSender, not a gateway.Sender: the OTP
// service only ever needs "deliver this code".
type fakeOtpSender struct{ codes []string }

func (s *fakeOtpSender) Send(_ context.Context, _ string, code string) error {
	s.codes = append(s.codes, code)
	return nil
}

func newTestOtpService(now time.Time) (*OtpService, *fakeStore, *fakeOtpSender) {
	store := &fakeStore{now: now}
	sender := &fakeOtpSender{}
	svc := NewOtpService(store, sender, 5*time.Minute)
	svc.now = func() time.Time { return now }
	svc.randomCode = func() (string, error) { return "123456", nil }
	return svc, store, sender
}

func kindOf(t *testing.T, err error) apperror.Kind {
	t.Helper()
	require.Error(t, err)
	var ae *apperror.Error
	require.True(t, errors.As(err, &ae), "expected *apperror.Error, got %T", err)
	return ae.Kind
}

func TestOtpRequest_ResendWithinCooldownRateLimited(t *testing.T) {
	now := time.Now()
	svc, _, sender := newTestOtpService(now)

	require.NoError(t, svc.Request(context.Background(), testPhone))
	err := svc.Request(context.Background(), testPhone)
	assert.Equal(t, apperror.KindTooManyRequests, kindOf(t, err))
	assert.Len(t, sender.codes, 1, "the throttled resend must not reach WhatsApp")

	// After 61 seconds a new code goes out.
	svc.now = func() time.Time { return now.Add(61 * time.Second) }
	require.NoError(t, svc.Request(context.Background(), testPhone))
	assert.Len(t, sender.codes, 2)
}

// A correct code proves the number once: the row is consumed, so a
// replayed code (an intercepted message, a resent form) is worthless.
func TestOtpCheck_HappyPathConsumesTheCode(t *testing.T) {
	now := time.Now()
	svc, _, _ := newTestOtpService(now)

	require.NoError(t, svc.Request(context.Background(), testPhone))
	require.NoError(t, svc.Check(context.Background(), testPhone, "123456"))

	err := svc.Check(context.Background(), testPhone, "123456")
	assert.Equal(t, apperror.KindBadRequest, kindOf(t, err))
}

func TestOtpCheck_ExpiredCodeRejected(t *testing.T) {
	now := time.Now()
	svc, _, _ := newTestOtpService(now)

	require.NoError(t, svc.Request(context.Background(), testPhone))
	svc.now = func() time.Time { return now.Add(6 * time.Minute) } // TTL is 5 min

	err := svc.Check(context.Background(), testPhone, "123456")
	assert.Equal(t, apperror.KindBadRequest, kindOf(t, err))
}

// Wrong codes count up; the 5th invalidates the code — even the
// correct code stops working afterwards (fail closed).
func TestOtpCheck_AttemptsExceededInvalidates(t *testing.T) {
	now := time.Now()
	svc, _, _ := newTestOtpService(now)

	require.NoError(t, svc.Request(context.Background(), testPhone))

	for i := 1; i <= 4; i++ {
		err := svc.Check(context.Background(), testPhone, "000000")
		assert.Equal(t, apperror.KindBadRequest, kindOf(t, err), "attempt %d", i)
	}

	// 5th bad try: code invalidated.
	err := svc.Check(context.Background(), testPhone, "000000")
	assert.Equal(t, apperror.KindBadRequest, kindOf(t, err))
	assert.Contains(t, err.Error(), "attempts_exceeded")

	// Even the correct code is dead now.
	err = svc.Check(context.Background(), testPhone, "123456")
	assert.Equal(t, apperror.KindBadRequest, kindOf(t, err))
}

// Four wrong tries followed by the right one still verifies: the limit
// exists to stop brute force, not to punish a typo.
func TestOtpCheck_FourWrongThenRight(t *testing.T) {
	now := time.Now()
	svc, _, _ := newTestOtpService(now)

	require.NoError(t, svc.Request(context.Background(), testPhone))
	for range 4 {
		err := svc.Check(context.Background(), testPhone, "000000")
		assert.Equal(t, apperror.KindBadRequest, kindOf(t, err))
	}
	require.NoError(t, svc.Check(context.Background(), testPhone, "123456"))
}
