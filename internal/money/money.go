// Package money formats and parses Indonesian Rupiah minor units for
// the bot. Formatting mirrors the dashboard law (`Rp8.003.500`, U+2212
// for negatives); parsing accepts the shorthand people actually type in
// WhatsApp (`25rb`, `2,5jt`, `Rp1.250.000`).
package money

import (
	"strconv"
	"strings"
)

// minusSign is U+2212 MINUS SIGN — the dashboard's negative marker, not
// the ASCII hyphen.
const minusSign = "\u2212"

// FormatIDR renders minor units as `Rp1.250.000`; negatives carry the
// minus sign ahead of the prefix.
func FormatIDR(minor int64) string {
	neg := minor < 0
	abs := minor
	if neg {
		abs = -minor
	}
	out := "Rp" + group(abs)
	if neg {
		return minusSign + out
	}
	return out
}

// FormatSigned always shows the direction: `+Rp8.000.000` / `−Rp25.000`.
// Zero has no direction, so it renders bare.
func FormatSigned(minor int64) string {
	switch {
	case minor > 0:
		return "+" + FormatIDR(minor)
	case minor < 0:
		return FormatIDR(minor)
	default:
		return FormatIDR(0)
	}
}

// group inserts '.' every three digits from the right.
func group(v int64) string {
	digits := strconv.FormatInt(v, 10)
	n := len(digits)
	if n <= 3 {
		return digits
	}
	var b strings.Builder
	b.Grow(n + (n-1)/3)
	lead := n % 3
	if lead > 0 {
		b.WriteString(digits[:lead])
	}
	for i := lead; i < n; i += 3 {
		if b.Len() > 0 {
			b.WriteByte('.')
		}
		b.WriteString(digits[i : i+3])
	}
	return b.String()
}

// maxAmount is the sanity ceiling: ten trillion rupiah is far past any
// personal-finance figure, so anything larger is a mis-parse.
const maxAmount = int64(10_000_000_000_000)

// multipliers maps the shorthand suffixes to their scale. Longest match
// wins, so "ribu" is tried before "rb".
var multipliers = []struct {
	suffix string
	scale  int64
}{
	{"ribu", 1_000},
	{"juta", 1_000_000},
	{"rb", 1_000},
	{"jt", 1_000_000},
	{"k", 1_000},
	{"m", 1_000_000},
}

// ParseToken parses one money token into minor units. It returns false
// for anything it cannot read exactly — the bot never guesses an amount.
//
//	"25rb" / "25k" → 25000        "2,5jt" / "2.5jt" → 2500000
//	"1.250.000"    → 1250000      "Rp25.000"        → 25000
//	"500 ribu"     → 500000       "2 juta"          → 2000000
func ParseToken(tok string) (int64, bool) {
	s := strings.ToLower(strings.TrimSpace(tok))
	s = strings.TrimPrefix(s, "rp")
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}

	scale := int64(1)
	for _, m := range multipliers {
		if strings.HasSuffix(s, m.suffix) {
			rest := strings.TrimSpace(strings.TrimSuffix(s, m.suffix))
			if rest == "" {
				continue
			}
			s = rest
			scale = m.scale
			break
		}
	}

	// A decimal fraction is only meaningful with a scale ("2,5jt"). A bare
	// "25.000" is thousands-grouped; a bare "25.000,50" would be
	// sub-rupiah cents, which IDR does not have — refuse rather than
	// silently reinterpret the comma as grouping.
	intPart, fracPart, hasSep := splitDecimal(s, scale > 1)
	if !hasSep {
		if scale == 1 && strings.Contains(s, ",") {
			return 0, false
		}
		intPart = strings.ReplaceAll(s, ".", "")
		fracPart = ""
	}
	if intPart == "" || !allDigits(intPart) || (fracPart != "" && !allDigits(fracPart)) {
		return 0, false
	}

	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, false
	}
	amount := whole * scale
	if fracPart != "" {
		frac, err := strconv.ParseInt(fracPart, 10, 64)
		if err != nil {
			return 0, false
		}
		div := int64(1)
		for range len(fracPart) {
			div *= 10
		}
		amount += frac * scale / div
	}
	if amount <= 0 || amount >= maxAmount {
		return 0, false
	}
	return amount, true
}

// splitDecimal separates "2,5" into ("2","5"). When scaled is false the
// separators are grouping marks and the caller strips them instead.
func splitDecimal(s string, scaled bool) (string, string, bool) {
	if !scaled {
		return "", "", false
	}
	i := strings.LastIndexAny(s, ".,")
	if i < 0 {
		return "", "", false
	}
	// Multiple separators means grouping ("1.250.000"), never a decimal.
	if strings.Count(s, ".")+strings.Count(s, ",") > 1 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
