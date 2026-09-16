// Package gateway is the outbound WhatsApp seam: one Sender interface
// with a real (gateway-backed) and a log-only implementation, plus the
// JID helpers every caller needs to compare phone identities.
package gateway

import "strings"

// DigitsOnly strips every non-digit rune. Phone identities arrive in
// three shapes (+62…, 62…, 0812…-ish); comparing digits is the only
// stable equality.
func DigitsOnly(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteByte(byte(r))
		}
	}
	return b.String()
}

// UserPart returns the user portion of a WhatsApp JID: the substring
// before the first ':' (device suffix) or '@' (server).
//
//	"628123456789:87@s.whatsapp.net" → "628123456789"
//	"628123456789@s.whatsapp.net"    → "628123456789"
func UserPart(jid string) string {
	if i := strings.IndexAny(jid, ":@"); i >= 0 {
		return jid[:i]
	}
	return jid
}
