package auth

import (
	"testing"
	"time"

	"github.com/glennprays/catet-backend/internal/apperror"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The epoch has to survive the round trip: it is the entire revocation
// mechanism. A token that came back at the wrong epoch — or lost it
// altogether — would keep a password change from signing devices out.
func TestToken_RoundTripCarriesUserAndEpoch(t *testing.T) {
	svc := NewTokenService("test-secret")
	userID := uuid.New()

	token, expiresAt, err := svc.Issue(userID, 7)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().Add(SessionTTL), expiresAt, time.Minute)

	sess, err := svc.Verify(token)
	require.NoError(t, err)
	assert.Equal(t, userID, sess.UserID)
	assert.Equal(t, 7, sess.Epoch)
	assert.WithinDuration(t, time.Now(), sess.IssuedAt, time.Minute)
}

func TestToken_ForeignSecretRejected(t *testing.T) {
	issuer := NewTokenService("test-secret")
	token, _, err := issuer.Issue(uuid.New(), 1)
	require.NoError(t, err)

	_, err = NewTokenService("another-secret").Verify(token)
	assert.Equal(t, apperror.KindUnauthorized, kindOf(t, err))
}

// An attacker who re-signs the claims with the real secret still cannot
// forge a *newer* epoch than the one they were given, but they can try
// an older one — so Verify must report the epoch it read rather than
// assume it, and RequireAuth compares it against the stored row.
func TestToken_TamperedEpochIsReportedNotTrusted(t *testing.T) {
	const secret = "test-secret"
	svc := NewTokenService(secret)
	userID := uuid.New()

	forged, err := jwt.NewWithClaims(jwt.SigningMethodHS256, sessionClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.String(),
			Issuer:    "catet-backend",
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(SessionTTL)),
		},
		Epoch: 99,
	}).SignedString([]byte(secret))
	require.NoError(t, err)

	sess, err := svc.Verify(forged)
	require.NoError(t, err, "a correctly signed token parses; the epoch is a claim, not a signature")
	assert.Equal(t, userID, sess.UserID)
	assert.Equal(t, 99, sess.Epoch, "the epoch must travel verbatim so the caller can compare it")
}

func TestToken_ExpiredRejected(t *testing.T) {
	const secret = "test-secret"
	expired, err := jwt.NewWithClaims(jwt.SigningMethodHS256, sessionClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   uuid.New().String(),
			Issuer:    "catet-backend",
			IssuedAt:  jwt.NewNumericDate(time.Now().Add(-2 * time.Hour)),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(-time.Hour)),
		},
		Epoch: 1,
	}).SignedString([]byte(secret))
	require.NoError(t, err)

	_, err = NewTokenService(secret).Verify(expired)
	assert.Equal(t, apperror.KindUnauthorized, kindOf(t, err))
}

// A subject that is not a uuid cannot name an account, so it must not
// reach the user lookup.
func TestToken_NonUUIDSubjectRejected(t *testing.T) {
	const secret = "test-secret"
	bogus, err := jwt.NewWithClaims(jwt.SigningMethodHS256, sessionClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "+628123456789",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
		Epoch: 1,
	}).SignedString([]byte(secret))
	require.NoError(t, err)

	_, err = NewTokenService(secret).Verify(bogus)
	assert.Equal(t, apperror.KindUnauthorized, kindOf(t, err))
}
