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

type fakeSender struct{ codes []string }

func (s *fakeSender) Send(_ context.Context, _ string, code string) error {
	s.codes = append(s.codes, code)
	return nil
}

func newTestOtpService(now time.Time) (*OtpService, *fakeStore, *fakeSender) {
	store := &fakeStore{now: now}
	sender := &fakeSender{}
	svc := NewOtpService(store, sender, NewTokenService("test-secret"), "+628000000000", 5*time.Minute)
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

func TestOtpRequest_WrongPhoneRejected(t *testing.T) {
	svc, _, _ := newTestOtpService(time.Now())
	err := svc.Request(context.Background(), "+628999999999")
	assert.Equal(t, apperror.KindBadRequest, kindOf(t, err))
}

func TestOtpRequest_ResendWithinCooldownRateLimited(t *testing.T) {
	now := time.Now()
	svc, _, _ := newTestOtpService(now)

	require.NoError(t, svc.Request(context.Background(), "+628000000000"))
	err := svc.Request(context.Background(), "+628000000000")
	assert.Equal(t, apperror.KindTooManyRequests, kindOf(t, err))

	// After 61 seconds a new code goes out.
	svc.now = func() time.Time { return now.Add(61 * time.Second) }
	require.NoError(t, svc.Request(context.Background(), "+628000000000"))
}

func TestOtpVerify_HappyPathIssuesToken(t *testing.T) {
	now := time.Now()
	svc, _, _ := newTestOtpService(now)

	require.NoError(t, svc.Request(context.Background(), "+628000000000"))
	token, expiresAt, err := svc.Verify(context.Background(), "+628000000000", "123456")
	require.NoError(t, err)
	assert.NotEmpty(t, token)
	assert.True(t, expiresAt.After(now))

	// The code is consumed: replaying fails.
	_, _, err = svc.Verify(context.Background(), "+628000000000", "123456")
	assert.Equal(t, apperror.KindBadRequest, kindOf(t, err))
}

func TestOtpVerify_ExpiredCodeRejected(t *testing.T) {
	now := time.Now()
	svc, _, _ := newTestOtpService(now)

	require.NoError(t, svc.Request(context.Background(), "+628000000000"))
	svc.now = func() time.Time { return now.Add(6 * time.Minute) } // TTL is 5 min

	_, _, err := svc.Verify(context.Background(), "+628000000000", "123456")
	assert.Equal(t, apperror.KindBadRequest, kindOf(t, err))
}

// Wrong codes count up; the 5th invalidates the code — even the
// correct code stops working afterwards (fail closed).
func TestOtpVerify_AttemptsExceededInvalidates(t *testing.T) {
	now := time.Now()
	svc, _, _ := newTestOtpService(now)

	require.NoError(t, svc.Request(context.Background(), "+628000000000"))

	for i := 1; i <= 4; i++ {
		_, _, err := svc.Verify(context.Background(), "+628000000000", "000000")
		assert.Equal(t, apperror.KindBadRequest, kindOf(t, err), "attempt %d", i)
	}

	// 5th bad try: code invalidated.
	_, _, err := svc.Verify(context.Background(), "+628000000000", "000000")
	assert.Equal(t, apperror.KindBadRequest, kindOf(t, err))
	assert.Contains(t, err.Error(), "attempts_exceeded")

	// Even the correct code is dead now.
	_, _, err = svc.Verify(context.Background(), "+628000000000", "123456")
	assert.Equal(t, apperror.KindBadRequest, kindOf(t, err))
}

// Four wrong tries followed by the right one still verifies.
func TestOtpVerify_FourWrongThenRight(t *testing.T) {
	now := time.Now()
	svc, _, _ := newTestOtpService(now)

	require.NoError(t, svc.Request(context.Background(), "+628000000000"))
	for range 4 {
		_, _, err := svc.Verify(context.Background(), "+628000000000", "000000")
		assert.Equal(t, apperror.KindBadRequest, kindOf(t, err))
	}
	_, _, err := svc.Verify(context.Background(), "+628000000000", "123456")
	require.NoError(t, err)
}
