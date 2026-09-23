package bot

import (
	"testing"
	"time"

	"github.com/glennprays/catet-backend/internal/bot/parse"
	"github.com/glennprays/catet-backend/internal/llm"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIntentDraftsMapParserSourcesByVerifiedPhysicalLine(t *testing.T) {
	now := time.Date(2026, time.September, 15, 15, 30, 0, 0, time.FixedZone("Asia/Jakarta", 7*60*60))
	text := "kopi 10k pakai dana\nteh 5k\nBRI"
	parsed := parse.Parse(text, now)
	require.Len(t, parsed.Drafts, 2)
	require.Equal(t, "bri", parsed.SourceFooter)

	modelSource := uuid.New()
	h := &Harness{}

	t.Run("partial later draft does not inherit first line source", func(t *testing.T) {
		got := h.intentDrafts([]llm.IntentDraft{{
			Type:       parse.TypeExpense,
			AmountText: "5k",
			Merchant:   "teh",
			SourceID:   &modelSource,
		}}, text, parse.LangID, parsed.Drafts)

		require.Len(t, got, 1)
		assert.Equal(t, int64(5_000), got[0].AmountMinor)
		assert.Empty(t, got[0].Pays, "the omitted first line's DANA hint must not leak")
		assert.Empty(t, got[0].Froms)
		assert.True(t, got[0].SourceFromParser, "the later model draft still matched its own physical line")
	})

	t.Run("reordered drafts retain their own line sources", func(t *testing.T) {
		got := h.intentDrafts([]llm.IntentDraft{
			{Type: parse.TypeExpense, AmountText: "5k", Merchant: "teh"},
			{Type: parse.TypeExpense, AmountText: "10k", Merchant: "kopi"},
		}, text, parse.LangID, parsed.Drafts)

		require.Len(t, got, 2)
		assert.Equal(t, "teh", got[0].Merchant)
		assert.Empty(t, got[0].Pays, "the shared BRI footer must remain applicable to the later line")
		assert.Equal(t, "kopi", got[1].Merchant)
		assert.Equal(t, []string{"dana"}, got[1].Pays, "explicit DANA must stay on the first physical line")
	})
}

func TestIntentDraftsClearUnprovenParserSourceHints(t *testing.T) {
	now := time.Date(2026, time.September, 15, 15, 30, 0, 0, time.FixedZone("Asia/Jakarta", 7*60*60))
	text := "kopi 10k pakai dana\nteh 10k\nBRI"
	parsed := parse.Parse(text, now)
	require.Len(t, parsed.Drafts, 2)

	got := (&Harness{}).intentDrafts([]llm.IntentDraft{{
		Type:       parse.TypeExpense,
		AmountText: "10k",
		Merchant:   "unknown merchant",
	}}, text, parse.LangID, parsed.Drafts)

	require.Len(t, got, 1)
	assert.Empty(t, got[0].Pays, "equal amounts without a merchant proof are not a physical-line match")
	assert.Empty(t, got[0].Froms)
	assert.False(t, got[0].SourceFromParser)
}
