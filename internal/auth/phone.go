package auth

import (
	"regexp"
	"strings"

	"github.com/glennprays/catet-backend/internal/apperror"
)

// e164 is the stored shape of every phone identity: a leading '+', a
// non-zero country digit, then 7-14 more digits.
var e164 = regexp.MustCompile(`^\+[1-9][0-9]{7,14}$`)

// NormalizePhone turns whatever the user typed into the one stored
// form. Indonesians write their own number four ways — 0812…, 62812…,
// +62 812-3456, (0812) 3456 — and all four are the same account, so the
// normalisation happens once here instead of at every comparison.
//
// A local 0-prefixed number is assumed Indonesian (+62): this is a
// single-country product, and guessing wrong is better caught by the
// user seeing a wrong number than by silently storing an ambiguous one.
func NormalizePhone(raw string) (string, error) {
	cleaned := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '-', '.', '(', ')', '\u00a0':
			return -1
		}
		return r
	}, strings.TrimSpace(raw))

	switch {
	case cleaned == "":
		return "", phoneError()
	case strings.HasPrefix(cleaned, "+"):
		// already international
	case strings.HasPrefix(cleaned, "0"):
		cleaned = "+62" + strings.TrimPrefix(cleaned, "0")
	default:
		cleaned = "+" + cleaned
	}

	if !e164.MatchString(cleaned) {
		return "", phoneError()
	}
	return cleaned, nil
}

func phoneError() *apperror.Error {
	return apperror.Validation("phone validation failed").WithDetails(apperror.FieldError{
		Field:   "phone",
		Rule:    "format",
		Message: "Nomor WhatsApp tidak valid",
	})
}
