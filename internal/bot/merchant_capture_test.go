package bot_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCaptureCanonicalMerchant is the whole point of canonical
// merchants, read from the chat: two spellings of the same idea become
// one merchant, the model is asked once, the phrase the user actually
// typed survives as the note, and the reply speaks the canonical name.
func TestCaptureCanonicalMerchant(t *testing.T) {
	script, client := newScriptedLLM(t)
	f := newBotFixture(t, client)
	script.setMerchant(`{"merchant_id":null,"name":"Traktir Teman","confidence":0.9}`)

	f.say(t, "m1", "traktir makan temen 133k pakai cash")

	txs := f.live(t)
	require.Len(t, txs, 1)
	require.NotNil(t, txs[0].Merchant)
	assert.Equal(t, "Traktir Teman", *txs[0].Merchant)
	require.NotNil(t, txs[0].MerchantID)
	require.NotNil(t, txs[0].Note)
	assert.Equal(t, "traktir makan temen", *txs[0].Note, "the typed phrase is not lost")
	assert.Contains(t, f.sender.last(), "Traktir Teman")
	assert.Equal(t, 1, script.callsOf("merchant_pick"))

	// A second spelling is a second phrase, so it costs one more call —
	// and the model folding it into the first merchant means one entity.
	first := *txs[0].MerchantID
	script.setMerchant(`{"merchant_id":"` + first.String() + `","name":"Traktir Teman","confidence":0.92}`)
	f.say(t, "m2", "traktir temen 50k pakai cash")

	txs = f.live(t)
	require.Len(t, txs, 2)
	require.NotNil(t, txs[1].MerchantID)
	assert.Equal(t, first, *txs[1].MerchantID)
	assert.Equal(t, 2, script.callsOf("merchant_pick"))

	// The remembered phrase never reaches the model again.
	f.say(t, "m3", "traktir temen 20k pakai cash")
	assert.Equal(t, 2, script.callsOf("merchant_pick"), "alias memory answers the repeat")

	txs = f.live(t)
	require.Len(t, txs, 3)
	require.NotNil(t, txs[2].MerchantID)
	assert.Equal(t, first, *txs[2].MerchantID)
}

// TestCaptureSurvivesMerchantModelFailure: naming is never allowed to
// block a capture. With the model refusing, the typed phrase is kept as
// the merchant, Title-Cased.
func TestCaptureSurvivesMerchantModelFailure(t *testing.T) {
	script, client := newScriptedLLM(t)
	f := newBotFixture(t, client)
	// An empty script body makes the fake answer with a 500-equivalent
	// unusable payload, which the client reports as an error.
	script.setMerchant(`not json at all`)

	f.say(t, "m1", "warung mbak yu 15k pakai cash")

	txs := f.live(t)
	require.Len(t, txs, 1)
	require.NotNil(t, txs[0].Merchant)
	assert.Equal(t, "Warung Mbak Yu", *txs[0].Merchant)
	require.NotNil(t, txs[0].MerchantID)
	assert.Equal(t, int64(15_000), txs[0].AmountMinor)
}
