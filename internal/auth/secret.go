package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// tokenBytes is the entropy behind an invite or reset link. 32 bytes is
// far beyond guessable and still fits a URL comfortably.
const tokenBytes = 32

// randomToken returns a URL-safe secret for an invite or a password
// reset. Only its hash is ever stored.
func randomToken() (string, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// HashToken is the stored form of a link secret. sha256 with no salt is
// right here — the input is 256 bits of randomness, so there is nothing
// to brute-force and a slow hash would only slow down the legitimate
// lookup.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
