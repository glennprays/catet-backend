// Package auth owns WhatsApp-OTP session auth: OTP delivery and
// verification, stateless HMAC-JWT session tokens, and the Fiber
// middleware that protects the API group.
package auth

import (
	"fmt"
	"time"

	"github.com/glennprays/catet-backend/internal/apperror"
	"github.com/golang-jwt/jwt/v5"
)

// SessionTTL is how long a session JWT stays valid. Logout clears the
// cookie only — there is no revocation list (single-user deployment;
// rotating AUTH_SECRET invalidates every session).
const SessionTTL = 30 * 24 * time.Hour

// TokenService issues and verifies HS256 session JWTs with claims
// {sub: phone, exp, iat}.
type TokenService struct {
	secret []byte
}

func NewTokenService(secret string) *TokenService {
	return &TokenService{secret: []byte(secret)}
}

// Issue mints a session token for the owner phone and returns it with
// its absolute expiry.
func (s *TokenService) Issue(phone string) (string, time.Time, error) {
	now := time.Now()
	expiresAt := now.Add(SessionTTL)
	claims := jwt.RegisteredClaims{
		Subject:   phone,
		Issuer:    "catet-backend",
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(expiresAt),
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign session token: %w", err)
	}
	return signed, expiresAt, nil
}

// Verify parses a signed token and returns the subject phone. Any
// failure collapses to UNAUTHORIZED.
func (s *TokenService) Verify(tokenStr string) (string, error) {
	token, err := jwt.ParseWithClaims(
		tokenStr,
		&jwt.RegisteredClaims{},
		func(t *jwt.Token) (any, error) { return s.secret, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
	)
	if err != nil || !token.Valid {
		return "", apperror.Unauthorized("invalid or expired session")
	}
	claims, ok := token.Claims.(*jwt.RegisteredClaims)
	if !ok || claims.Subject == "" {
		return "", apperror.Unauthorized("invalid or expired session")
	}
	return claims.Subject, nil
}
