// Package secretbox seals a short secret at rest with AES-256-GCM.
//
// It exists for exactly one problem: the admin's LLM API key has to be
// stored in Postgres and handed back to the HTTP client that calls the
// provider, so it can be neither hashed (auth/secret.go's job, one-way)
// nor kept in plaintext in a column that ends up in every dump.
//
// The key is derived from AUTH_SECRET so a deployment already has one
// secret to protect, not two. The derivation is domain-separated: the
// same AUTH_SECRET that signs session JWTs must not produce the same
// bytes here. Rotating AUTH_SECRET therefore invalidates stored
// ciphertext — Open returns an error and the admin re-enters the key.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
)

// derivationLabel domain-separates this key from every other use of the
// same secret. Changing it is a silent data loss, so it never changes.
const derivationLabel = "catet/llm-provider/"

// Box seals and opens one secret. It is immutable and safe for
// concurrent use: the AEAD is stateless once built.
type Box struct {
	aead cipher.AEAD
}

// New derives the box from the instance secret. An AES-256 key is
// exactly the width of a SHA-256 digest, so the derivation is a hash,
// not a KDF with parameters to get wrong.
func New(secret string) Box {
	key := sha256.Sum256([]byte(derivationLabel + secret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		// Unreachable: the key is always 32 bytes.
		panic(fmt.Sprintf("secretbox: aes cipher: %v", err))
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		panic(fmt.Sprintf("secretbox: gcm: %v", err))
	}
	return Box{aead: aead}
}

// Seal returns base64(nonce || ciphertext). An empty plaintext seals to
// an empty string: "no key configured" must stay recognisable in the
// column without decrypting anything.
func (b Box) Seal(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	if b.aead == nil {
		return "", errors.New("secretbox: zero box")
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("secretbox: nonce: %w", err)
	}
	sealed := b.aead.Seal(nonce, nonce, []byte(plain), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Open reverses Seal. An empty input is the empty secret, not an error;
// anything that fails to authenticate is an error, never a partial
// answer.
func (b Box) Open(enc string) (string, error) {
	if enc == "" {
		return "", nil
	}
	if b.aead == nil {
		return "", errors.New("secretbox: zero box")
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", fmt.Errorf("secretbox: decode: %w", err)
	}
	n := b.aead.NonceSize()
	if len(raw) < n {
		return "", errors.New("secretbox: ciphertext too short")
	}
	plain, err := b.aead.Open(nil, raw[:n], raw[n:], nil)
	if err != nil {
		return "", fmt.Errorf("secretbox: open: %w", err)
	}
	return string(plain), nil
}
