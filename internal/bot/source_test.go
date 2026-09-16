package bot

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// cashLike is the rule that stopped the bot filing a card payment under
// Cash: a register prints TUNAI for anything it does not itemise, so the
// word is never allowed to decide a Source on its own — and, just as
// importantly, never pays for an LLM alias repair.
func TestCashLikePaymentMethods(t *testing.T) {
	for _, printed := range []string{"TUNAI", "tunai", "CASH", "Cash Payment", "KAS"} {
		assert.True(t, cashLike.MatchString(printed), "%q should read as cash", printed)
	}
	for _, printed := range []string{"DEBIT BCA", "QRIS", "GOPAY", "Kartu Kredit"} {
		assert.False(t, cashLike.MatchString(printed), "%q is a real payment method", printed)
	}
}
