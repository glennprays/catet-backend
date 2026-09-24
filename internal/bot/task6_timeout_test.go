package bot_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnrichmentTimeoutKeepsRawMerchantCaptureWritable(t *testing.T) {
	script, client := newScriptedLLMWithTimeout(t, 10*time.Second)
	script.blockCalls()
	f := newBotFixture(t, client)
	f.say(t, "m-enrich-merchant", "traktir temen 10k pakai cash")

	assert.Equal(t, 1, script.callsOf("merchant_pick"), "merchant enrichment makes one bounded call")
	assert.Less(t, script.requestDeadline(), 6*time.Second, "provider request receives the five-second enrichment deadline")
	assert.Equal(t, 1, script.cancellationsOf("merchant_pick"), "provider observes enrichment cancellation")

	txs := f.live(t)
	require.Len(t, txs, 1)
	require.NotNil(t, txs[0].Merchant)
	assert.Equal(t, "Traktir Temen", *txs[0].Merchant)
	assert.NotEmpty(t, f.sender.last(), "the deterministic ledger result must be delivered")
}

func TestEnrichmentTimeoutOnUnmatchedSourceLeavesHoldWithoutDefault(t *testing.T) {
	script, client := newScriptedLLMWithTimeout(t, 10*time.Second)
	script.blockCalls()
	f := newBotFixture(t, client)

	f.say(t, "m-enrich-source", "kopi 10k pakai not-a-source")

	assert.Equal(t, 1, script.callsOf("source_pick"), "unmatched Source alias makes one bounded repair call")
	assert.Less(t, script.requestDeadline(), 6*time.Second, "provider request receives the five-second enrichment deadline")
	assert.Equal(t, 1, script.cancellationsOf("source_pick"), "provider observes enrichment cancellation")

	assert.Empty(t, f.live(t), "an unmatched Source alias must never write without a Source")
	assert.Equal(t, 1, f.openActions(t))
	assert.Contains(t, f.sender.last(), "sumber")
}
func TestEnrichmentTimeoutOnCategoryStillWritesNeedsReview(t *testing.T) {
	script, client := newScriptedLLMWithTimeout(t, 100*time.Millisecond)
	script.blockCalls()
	f := newBotFixture(t, client)

	f.say(t, "m-enrich-category", "10k pakai cash")
	assert.Less(t, script.requestDeadline(), 6*time.Second, "provider request receives the five-second enrichment deadline")
	assert.Equal(t, 1, script.cancellationsOf("category_pick"), "provider observes enrichment cancellation")

	assert.Equal(t, 1, script.callsOf("category_pick"), "category enrichment makes one bounded call")

	txs := f.live(t)
	require.Len(t, txs, 1)
	assert.Nil(t, txs[0].CategoryID)
	assert.Equal(t, "needs_review", txs[0].ReviewStatus)
	assert.NotEmpty(t, f.sender.last(), "the deterministic ledger result must be delivered")
}

func TestIntentProviderExhaustionStillStoresAndDeliversReply(t *testing.T) {
	script, client := newScriptedLLMWithTimeout(t, 100*time.Millisecond)
	script.blockCalls()
	script.setIntent(`{"intent":"unknown","drafts":[],"correction":null,"undo_index":0,"period":"","balance_source_id":null,"answer":null}`)
	f := newBotFixture(t, client)
	f.say(t, "m-intent-timeout", "tolong bantu")

	assert.Equal(t, 2, script.callsOf("intent"), "provider exhaustion consumes both bounded attempts")
	status, _ := f.inboundRow(t, "m-intent-timeout")
	assert.Equal(t, "replied", status)
	assert.NotEmpty(t, f.sender.last(), "intent failure must not leave only the seen reaction")
}

func TestDeterministicFooterCaptureSkipsOptionalEnrichment(t *testing.T) {
	script, client := newScriptedLLM(t)
	f := newBotFixture(t, client)
	script.setCategory(`{"category_id":"` + f.makanan.ID.String() + `","confidence":0.9}`)

	f.say(t, "m-footer-no-ai", "sarapan bubur ayam 16k\ncilok 10k\nBRI")

	txs := f.live(t)
	require.Len(t, txs, 2)
	for _, tx := range txs {
		require.NotNil(t, tx.CategoryID)
		assert.Equal(t, f.makanan.ID, *tx.CategoryID)
		require.NotNil(t, tx.CategorySource)
		assert.Equal(t, "llm", *tx.CategorySource)
		assert.Equal(t, "clean", tx.ReviewStatus)
		assert.Equal(t, f.bri.ID, legSourceID(t, tx.Legs))
	}
	assert.Equal(t, 0, script.callsOf("merchant_pick"))
	assert.Equal(t, 2, script.callsOf("category_pick"))
	assert.Equal(t, 0, script.callsOf("intent"))
	assert.Equal(t, 0, script.callsOf("source_pick"))
}

func TestCategoryDeadlineIsIndependentAfterMerchantAuxiliaryTimeout(t *testing.T) {
	script, client := newScriptedLLMWithTimeout(t, 10*time.Second)
	f := newBotFixture(t, client)
	script.setCategory(`{"category_id":"` + f.makanan.ID.String() + `","confidence":0.9}`)
	script.blockOperation("merchant_pick")

	f.say(t, "m-category-after-merchant", "nasgor 18k bri")

	txs := f.live(t)
	require.Len(t, txs, 1)
	assert.Equal(t, 1, script.callsOf("category_pick"))
	require.NotNil(t, txs[0].CategoryID)
	require.NotNil(t, txs[0].CategorySource)
	assert.Equal(t, "llm", *txs[0].CategorySource)
	assert.Equal(t, "clean", txs[0].ReviewStatus)
	assert.Equal(t, 1, script.callsOf("merchant_pick"))
	assert.Equal(t, 1, script.cancellationsOf("merchant_pick"))
	assert.Equal(t, 0, script.cancellationsOf("category_pick"))
}

func TestLowConfidenceCategoryStillWritesNeedsReview(t *testing.T) {
	script, client := newScriptedLLM(t)
	f := newBotFixture(t, client)
	script.setCategory(`{"category_id":"` + f.makanan.ID.String() + `","confidence":0.4}`)

	f.say(t, "m-low-confidence", "kopi 10k cash")

	txs := f.live(t)
	require.Len(t, txs, 1)
	require.NotNil(t, txs[0].CategoryID)
	assert.Equal(t, f.makanan.ID, *txs[0].CategoryID)
	require.NotNil(t, txs[0].CategorySource)
	assert.Equal(t, "llm", *txs[0].CategorySource)
	assert.Equal(t, "needs_review", txs[0].ReviewStatus)
}

func TestCategoryCandidatesGuardNullUnknownAndEmptyLists(t *testing.T) {
	tests := []struct {
		name       string
		response   func(*botFixture) string
		archiveAll bool
	}{
		{
			name: "null model id",
			response: func(*botFixture) string {
				return `{"category_id":null,"confidence":0}`
			},
		},
		{
			name: "unknown model id",
			response: func(*botFixture) string {
				return `{"category_id":"` + uuid.New().String() + `","confidence":0.99}`
			},
		},
		{
			name: "no active categories",
			response: func(*botFixture) string {
				return `{"category_id":null,"confidence":0}`
			},
			archiveAll: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			script, client := newScriptedLLM(t)
			f := newBotFixture(t, client)
			script.setCategory(tc.response(f))
			if tc.archiveAll {
				_, err := f.pool.Exec(f.ctx, `UPDATE categories SET archived_at = now() WHERE user_id = $1`, f.userID)
				require.NoError(t, err)
			}

			f.say(t, "m-category-edge", "kopi 10k cash")

			txs := f.live(t)
			require.Len(t, txs, 1)
			assert.Nil(t, txs[0].CategoryID)
			assert.Nil(t, txs[0].CategorySource)
			assert.Equal(t, "needs_review", txs[0].ReviewStatus)
			if tc.archiveAll {
				assert.Equal(t, 0, script.callsOf("category_pick"))
			} else {
				assert.Equal(t, 1, script.callsOf("category_pick"))
			}
		})
	}
}
