package bot_test

import (
	"testing"
	"time"

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
	script, client := newScriptedLLMWithTimeout(t, 10*time.Second)
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

	f.say(t, "m-footer-no-ai", "sarapan bubur ayam 16k\ncilok 10k\nBRI")

	assert.Len(t, f.live(t), 2)
	assert.Equal(t, 0, script.callsOf("merchant_pick"))
	assert.Equal(t, 0, script.callsOf("category_pick"))
	assert.Equal(t, 0, script.callsOf("intent"))
	assert.Equal(t, 0, script.callsOf("source_pick"))
}
