package auth

import (
	"errors"
	"strings"
	"testing"

	"github.com/glennprays/catet-backend/internal/apperror"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHashPassword_RoundTrip(t *testing.T) {
	const pw = "sangat-rahasia-2026"

	encoded, err := HashPassword(pw)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(encoded, "$argon2id$"),
		"the stored encoding must name its scheme so the cost can be raised later: %q", encoded)

	ok, err := VerifyPassword(encoded, pw)
	require.NoError(t, err)
	assert.True(t, ok)

	// Two hashes of the same password differ: the salt is per-hash, so
	// the table cannot be attacked with one precomputed dictionary.
	other, err := HashPassword(pw)
	require.NoError(t, err)
	assert.NotEqual(t, encoded, other)
}

func TestVerifyPassword_WrongPasswordIsNotAnError(t *testing.T) {
	encoded, err := HashPassword("sangat-rahasia-2026")
	require.NoError(t, err)

	ok, err := VerifyPassword(encoded, "sangat-rahasia-2025")
	require.NoError(t, err, "a wrong password is an answer, not a failure")
	assert.False(t, ok)
}

// A hash we cannot verify must be reported as such. Collapsing it into
// "wrong password" would let a corrupted or downgraded row look like a
// user error forever, and would hide a scheme nobody meant to accept.
func TestVerifyPassword_ForeignEncodingIsErrBadHash(t *testing.T) {
	argon, err := HashPassword("sangat-rahasia-2026")
	require.NoError(t, err)

	cases := []struct {
		name    string
		encoded string
	}{
		{"bcrypt", "$2y$10$abcdefghijklmnopqrstuvwxyz012345678901234567890123456"},
		{"argon2i", strings.Replace(argon, "argon2id", "argon2i", 1)},
		// A row that lost its digest: parseable prefix, nothing to compare.
		{"truncated", strings.Join(strings.Split(argon, "$")[:5], "$")},
		{"empty", ""},
		{"plaintext", "sangat-rahasia-2026"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, err := VerifyPassword(tc.encoded, "sangat-rahasia-2026")
			assert.False(t, ok)
			assert.True(t, errors.Is(err, ErrBadHash), "want ErrBadHash, got %v", err)
		})
	}
}

// The bounds are counted in runes: a 10-emoji password is 10 characters
// to the person typing it, and counting bytes would reject it.
func TestValidatePassword_LengthBoundsCountRunes(t *testing.T) {
	cases := []struct {
		name string
		pw   string
		rule string // "" = accepted
	}{
		{"nine runes", strings.Repeat("a", 9), "min"},
		{"ten runes", strings.Repeat("a", 10), ""},
		{"nine multibyte runes", strings.Repeat("é", 9), "min"},
		{"ten multibyte runes", strings.Repeat("é", 10), ""},
		{"128 runes", strings.Repeat("a", 128), ""},
		{"129 runes", strings.Repeat("a", 129), "max"},
		{"128 multibyte runes", strings.Repeat("π", 128), ""},
		{"129 multibyte runes", strings.Repeat("π", 129), "max"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePassword(tc.pw)
			if tc.rule == "" {
				assert.NoError(t, err)
				return
			}
			var ae *apperror.Error
			require.True(t, errors.As(err, &ae), "want *apperror.Error, got %v", err)
			assert.Equal(t, apperror.KindValidation, ae.Kind)
			require.Len(t, ae.Details, 1)
			assert.Equal(t, "password", ae.Details[0].Field)
			assert.Equal(t, tc.rule, ae.Details[0].Rule)
		})
	}
}

// "qwertyuiop" clears the length floor, so only the blocklist can
// reject it — which is the whole point of shipping the list.
func TestValidatePassword_CommonPasswordRejected(t *testing.T) {
	for _, pw := range []string{"qwertyuiop", "QwertyuioP", "1234567890"} {
		err := ValidatePassword(pw)

		var ae *apperror.Error
		require.True(t, errors.As(err, &ae), "%q should be rejected, got %v", pw, err)
		assert.Equal(t, apperror.KindValidation, ae.Kind)
		require.Len(t, ae.Details, 1)
		assert.Equal(t, "password", ae.Details[0].Field)
		assert.Equal(t, "common", ae.Details[0].Rule,
			"%q is on the 10k list; case must not smuggle it past", pw)
	}
}
