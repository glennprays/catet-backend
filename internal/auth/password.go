package auth

import (
	"bufio"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/glennprays/catet-backend/internal/apperror"
	"golang.org/x/crypto/argon2"
)

// argon2id parameters. Memory dominates the cost on a self-hosted box:
// 64 MiB × 3 passes is ~100 ms on the target hardware, which is slow
// enough to make an offline crack expensive and fast enough that a
// login does not feel stuck.
const (
	argonTime    uint32 = 3
	argonMemory  uint32 = 64 * 1024 // KiB
	argonThreads uint8  = 2
	argonSaltLen        = 16
	argonKeyLen  uint32 = 32
)

// Password length bounds. The floor is what makes the common-password
// list meaningful; the ceiling only exists so a megabyte of input
// cannot turn a login into a denial of service.
const (
	minPasswordRunes = 10
	maxPasswordRunes = 128
)

//go:embed common_passwords.txt
var commonPasswordsFS embed.FS

// commonPasswords is the 10k-most-common list (SecLists
// Passwords/Common-Credentials/10k-most-common.txt), lowercased. A
// 10-character minimum keeps out short passwords; this keeps out long
// ones everybody already guesses.
var commonPasswords map[string]struct{}

func init() {
	f, err := commonPasswordsFS.Open("common_passwords.txt")
	if err != nil {
		panic(fmt.Sprintf("auth: open common password list: %v", err))
	}
	defer func() { _ = f.Close() }()

	commonPasswords = make(map[string]struct{}, 10_500)
	s := bufio.NewScanner(f)
	for s.Scan() {
		if line := strings.TrimSpace(s.Text()); line != "" {
			commonPasswords[strings.ToLower(line)] = struct{}{}
		}
	}
	if err := s.Err(); err != nil {
		panic(fmt.Sprintf("auth: read common password list: %v", err))
	}
}

// ValidatePassword enforces the policy before anything is hashed, so
// the rejection costs nothing and names the rule the user broke.
func ValidatePassword(pw string) error {
	n := utf8.RuneCountInString(pw)
	switch {
	case n < minPasswordRunes:
		return passwordError("min", fmt.Sprintf("Kata sandi minimal %d karakter", minPasswordRunes))
	case n > maxPasswordRunes:
		return passwordError("max", fmt.Sprintf("Kata sandi maksimal %d karakter", maxPasswordRunes))
	}
	if _, common := commonPasswords[strings.ToLower(pw)]; common {
		return passwordError("common", "Kata sandi terlalu umum")
	}
	return nil
}

func passwordError(rule, msg string) *apperror.Error {
	return apperror.Validation("password validation failed").WithDetails(apperror.FieldError{
		Field:   "password",
		Rule:    rule,
		Message: msg,
	})
}

// HashPassword returns the PHC-style encoding stored in
// users.password_hash: `$argon2id$v=19$m=…,t=…,p=…$<salt>$<hash>`. The
// parameters travel with the hash so raising the cost later does not
// invalidate existing passwords.
func HashPassword(pw string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// ErrBadHash means the stored encoding is not one we can verify — a
// corrupted row or a hash from another scheme. It is never a wrong
// password, so callers must not report it as one.
var ErrBadHash = errors.New("unsupported password hash encoding")

// VerifyPassword recomputes the hash with the stored parameters and
// compares in constant time. Only argon2id is accepted: silently
// honouring another variant would let a downgraded row weaken a
// password.
func VerifyPassword(encoded, pw string) (bool, error) {
	parts := strings.Split(encoded, "$")
	// ["", "argon2id", "v=19", "m=…,t=…,p=…", salt, hash]
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return false, ErrBadHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, ErrBadHash
	}
	var (
		memory  uint32
		time    uint32
		threads uint8
	)
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &threads); err != nil {
		return false, ErrBadHash
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, ErrBadHash
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false, ErrBadHash
	}

	got := argon2.IDKey([]byte(pw), salt, time, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
