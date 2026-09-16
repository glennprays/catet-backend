// Package auth owns account authentication: phone + password login,
// WhatsApp OTP for signup verification and password reset, invites,
// stateless HMAC-JWT session tokens, and the Fiber middleware that
// protects the API group.
package auth

import (
	"fmt"
	"time"

	"github.com/glennprays/catet-backend/internal/apperror"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// SessionTTL is how long a session JWT stays valid. There is still no
// revocation list: revocation is users.session_epoch, which every token
// carries and RequireAuth re-checks on every request — so a password
// change or "log out everywhere" invalidates issued tokens without
// storing them.
const SessionTTL = 30 * 24 * time.Hour

// RenewAfter is how old a presented token may get before RequireAuth
// answers with a freshly minted one. Sessions slide: a device that
// keeps using the dashboard never hits the 30-day wall, while
// session_epoch stays the only revocation lever.
const RenewAfter = 7 * 24 * time.Hour

// RenewHeader carries that fresh token to the dashboard BFF, which
// re-sets its httpOnly cookie.
const RenewHeader = "X-Session-Renew"

// Session is what a verified token names: the account, the epoch it was
// issued under, and when it was issued (the input to the renewal rule).
type Session struct {
	UserID   uuid.UUID
	Epoch    int
	IssuedAt time.Time
}

// sessionClaims is the token payload: which account, and which epoch of
// that account's sessions it belongs to.
type sessionClaims struct {
	jwt.RegisteredClaims
	Epoch int `json:"epoch"`
}

// TokenService issues and verifies HS256 session JWTs.
type TokenService struct {
	secret []byte
}

func NewTokenService(secret string) *TokenService {
	return &TokenService{secret: []byte(secret)}
}

// Issue mints a session token for one account at its current epoch and
// returns it with its absolute expiry.
func (s *TokenService) Issue(userID uuid.UUID, epoch int) (string, time.Time, error) {
	return s.issueAt(userID, epoch, time.Now())
}

// issueAt is Issue with the clock injected, so tests can mint a token
// whose iat/exp are anchored in the past.
func (s *TokenService) issueAt(userID uuid.UUID, epoch int, now time.Time) (string, time.Time, error) {
	expiresAt := now.Add(SessionTTL)
	claims := sessionClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.String(),
			Issuer:    "catet-backend",
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
		},
		Epoch: epoch,
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign session token: %w", err)
	}
	return signed, expiresAt, nil
}

// Verify parses a signed token and returns the session it names: the
// account, the epoch it was issued under, and its issue time. Any
// failure collapses to UNAUTHORIZED: a caller must not be able to tell
// a bad signature from an expired token.
func (s *TokenService) Verify(tokenStr string) (Session, error) {
	token, err := jwt.ParseWithClaims(
		tokenStr,
		&sessionClaims{},
		func(t *jwt.Token) (any, error) { return s.secret, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
	)
	if err != nil || !token.Valid {
		return Session{}, apperror.Unauthorized("invalid or expired session")
	}
	claims, ok := token.Claims.(*sessionClaims)
	if !ok {
		return Session{}, apperror.Unauthorized("invalid or expired session")
	}
	userID, err := uuid.Parse(claims.Subject)
	if err != nil {
		return Session{}, apperror.Unauthorized("invalid or expired session")
	}
	// Without an iat there is no age to slide on, and a token this
	// service did not mint has no business being honoured.
	if claims.IssuedAt == nil || claims.IssuedAt.IsZero() {
		return Session{}, apperror.Unauthorized("invalid or expired session")
	}
	return Session{UserID: userID, Epoch: claims.Epoch, IssuedAt: claims.IssuedAt.Time}, nil
}
