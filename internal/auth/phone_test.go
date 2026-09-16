package auth

import (
	"errors"
	"testing"

	"github.com/glennprays/catet-backend/internal/apperror"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every way an Indonesian writes their own number has to collapse to
// the one stored form. The phone *is* the account identity, so two
// spellings reaching two rows would be two accounts for one person —
// and the bot, which matches on digits, would answer neither reliably.
func TestNormalizePhone_AllSpellingsCollapseToOneForm(t *testing.T) {
	const want = "+628123456789"

	cases := []struct {
		name string
		in   string
	}{
		{"local zero prefix", "08123456789"},
		{"country code without plus", "628123456789"},
		{"international with separators", "+62 812-3456-789"},
		{"local in parentheses", "(0812) 3456 789"},
		{"already normalised", "+628123456789"},
		{"surrounding whitespace", "  08123456789 "},
		{"dotted", "0812.3456.789"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizePhone(tc.in)
			require.NoError(t, err)
			assert.Equal(t, want, got)
		})
	}
}

func TestNormalizePhone_Rejects(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"whitespace only", "   "},
		{"separators only", "()- ."},
		{"zero country code", "+0812345678"},
		{"too short", "+621234"},
		{"too long", "+6281234567890123"},
		{"letters", "+62812ABCDEF"},
		{"bootstrap placeholder", BootstrapPhone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NormalizePhone(tc.in)

			var ae *apperror.Error
			require.True(t, errors.As(err, &ae), "want *apperror.Error, got %v", err)
			assert.Equal(t, apperror.KindValidation, ae.Kind)
			require.Len(t, ae.Details, 1)
			// The dashboard highlights the field it names, so the field
			// and rule are part of the contract, not decoration.
			assert.Equal(t, "phone", ae.Details[0].Field)
			assert.Equal(t, "format", ae.Details[0].Rule)
		})
	}
}
