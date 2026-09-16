package bot_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glennprays/catet-backend/internal/llm"
	"github.com/glennprays/catet-backend/internal/money"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scriptedLLM answers each of the bot's three prompts from its own
// script. The schema name travels in the system prompt, which is how one
// endpoint can stand in for the intent router and the category picker at
// the same time.
type scriptedLLM struct {
	mu       sync.Mutex
	intent   string
	category string
	source   string
	calls    map[string]int
	prompts  []string
}

func newScriptedLLM(t *testing.T) (*scriptedLLM, *llm.Client) {
	t.Helper()
	s := &scriptedLLM{
		category: `{"category_id":null,"confidence":0}`,
		source:   `{"source_id":null}`,
		calls:    map[string]int{},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Len(t, body.Messages, 2)

		s.mu.Lock()
		kind := "unknown"
		switch {
		case strings.Contains(body.Messages[0].Content, "(intent)"):
			kind = "intent"
		case strings.Contains(body.Messages[0].Content, "(category_pick)"):
			kind = "category_pick"
		case strings.Contains(body.Messages[0].Content, "(source_pick)"):
			kind = "source_pick"
		}
		s.calls[kind]++
		s.prompts = append(s.prompts, body.Messages[1].Content)
		content := map[string]string{
			"intent": s.intent, "category_pick": s.category, "source_pick": s.source,
		}[kind]
		s.mu.Unlock()

		require.NotEmpty(t, content, "no script for %s prompt", kind)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": content}}},
		})
	}))
	t.Cleanup(srv.Close)
	return s, llm.New(srv.URL, "test-key", "test-model", 2*time.Second)
}

func (s *scriptedLLM) setIntent(body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.intent = body
}

func (s *scriptedLLM) setCategory(body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.category = body
}

func (s *scriptedLLM) callsOf(kind string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[kind]
}

func (s *scriptedLLM) lastPrompt() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.prompts) == 0 {
		return ""
	}
	return s.prompts[len(s.prompts)-1]
}

func (f *botFixture) openActions(t *testing.T) int {
	t.Helper()
	var n int
	require.NoError(t, f.pool.QueryRow(f.ctx,
		`SELECT COUNT(*) FROM pending_actions WHERE resolved_at IS NULL`).Scan(&n))
	return n
}

// An answered hold must still be categorised: the parked draft never
// passed through the inline resolution, so without this the write lands
// as "belum ada kategori" even though the merchant is obvious.
func TestAnsweredHoldIsCategorised(t *testing.T) {
	script, client := newScriptedLLM(t)
	f := newBotFixture(t, client)
	script.setCategory(`{"category_id":"` + f.makanan.ID.String() + `","confidence":0.9}`)

	f.say(t, "m1", "ketang goreng 10k")
	assert.Contains(t, f.sender.last(), "Bayar pakai sumber mana?")
	assert.Empty(t, f.live(t))

	f.say(t, "m2", "bca")

	txs := f.live(t)
	require.Len(t, txs, 1)
	require.NotNil(t, txs[0].CategoryID)
	assert.Equal(t, f.makanan.ID, *txs[0].CategoryID)
	require.NotNil(t, txs[0].CategorySource)
	assert.Equal(t, "llm", *txs[0].CategorySource)
	assert.Equal(t, "clean", txs[0].ReviewStatus)

	reply := f.sender.last()
	assert.Contains(t, reply, "Makanan & Minuman")
	assert.Contains(t, reply, "BCA")
	// Answering a question is a rules match: no router round trip.
	assert.Equal(t, 0, script.callsOf("intent"))
}

// A plain capture never pays for the router.
func TestPlainCaptureSkipsTheIntentRouter(t *testing.T) {
	script, client := newScriptedLLM(t)
	f := newBotFixture(t, client)

	f.say(t, "m1", "ayam crispi 25k dari bca")

	require.Len(t, f.live(t), 1)
	assert.Equal(t, 0, script.callsOf("intent"))
	assert.Equal(t, 1, script.callsOf("category_pick"))
}

func TestCorrectionRelabelsTheLastTransaction(t *testing.T) {
	script, client := newScriptedLLM(t)
	f := newBotFixture(t, client)

	f.say(t, "m1", "ayam crispi 25k dari bca")
	original := f.live(t)[0]
	require.Nil(t, original.CategoryID)

	script.setIntent(`{"intent":"correct","drafts":[],"correction":{"target_index":0,"category_id":"` +
		f.makanan.ID.String() + `","source_id":null,"merchant":null,"amount_text":null,"day_offset":null},` +
		`"undo_index":0,"period":"","balance_source_id":null,"answer":null}`)
	f.say(t, "m2", "itu kategori makanan & minuman")

	// The router saw the batch it is correcting.
	assert.Contains(t, script.lastPrompt(), "ayam crispi")

	txs := f.live(t)
	require.Len(t, txs, 1, "a correction edits in place, it does not write a second row")
	assert.Equal(t, original.ID, txs[0].ID)
	require.NotNil(t, txs[0].CategoryID)
	assert.Equal(t, f.makanan.ID, *txs[0].CategoryID)
	require.NotNil(t, txs[0].CategorySource)
	assert.Equal(t, "user", *txs[0].CategorySource)
	assert.Equal(t, "clean", txs[0].ReviewStatus)
	assert.Equal(t, 0, f.openActions(t), "a correction parks nothing")

	assert.Equal(t, "Oke, kuperbaiki: Rp25.000 · ayam crispi · Makanan & Minuman · BCA", f.sender.last())
}

func TestSourceCorrectionReversesAndRewrites(t *testing.T) {
	script, client := newScriptedLLM(t)
	f := newBotFixture(t, client)

	f.say(t, "m1", "ayam crispi 25k dari bca")
	original := f.live(t)[0]

	script.setIntent(`{"intent":"correct","drafts":[],"correction":{"target_index":0,"category_id":null,` +
		`"source_id":"` + f.gopay.ID.String() + `","merchant":null,"amount_text":null,"day_offset":null},` +
		`"undo_index":0,"period":"","balance_source_id":null,"answer":null}`)
	f.say(t, "m2", "itu dari gopay bukan bca")

	// The original is reversed by a linked row, and the replacement is a
	// normal capture on the new source.
	var reverses int
	require.NoError(t, f.pool.QueryRow(f.ctx,
		`SELECT COUNT(*) FROM transactions WHERE reverses_transaction_id = $1`, original.ID).Scan(&reverses))
	assert.Equal(t, 1, reverses)

	live := f.live(t)
	var replacement uuid.UUID
	for _, tx := range live {
		if tx.ExternalID != nil && *tx.ExternalID == "m2#1" {
			replacement = tx.ID
			assert.Equal(t, "whatsapp_text", tx.Origin)
			assert.Equal(t, int64(25_000), tx.AmountMinor)
			assert.Equal(t, f.gopay.ID, legSourceID(t, tx.Legs))
		}
	}
	require.NotEqual(t, uuid.Nil, replacement, "the rewrite is missing")
	assert.Contains(t, f.sender.last(), "Oke, kuperbaiki: Rp25.000 · ayam crispi")
	assert.Contains(t, f.sender.last(), "GoPay")

	// The batch is addressable again: "batal" must reverse the rewrite,
	// not the row that no longer exists.
	batch, err := f.inbound.Get(f.ctx, "m1")
	require.NoError(t, err)
	assert.Contains(t, batch.TransactionIDs, replacement)
	assert.NotContains(t, batch.TransactionIDs, original.ID)

	f.say(t, "m3", "batal")
	require.NoError(t, f.pool.QueryRow(f.ctx,
		`SELECT COUNT(*) FROM transactions WHERE reverses_transaction_id = $1`, replacement).Scan(&reverses))
	assert.Equal(t, 1, reverses)
}

func TestBalanceQuestionIsAnswered(t *testing.T) {
	script, client := newScriptedLLM(t)
	f := newBotFixture(t, client)

	f.say(t, "m1", "ayam crispi 25k dari bca")

	script.setIntent(`{"intent":"balance","drafts":[],"correction":null,"undo_index":0,"period":"",` +
		`"balance_source_id":"` + f.bca.ID.String() + `","answer":null}`)
	f.say(t, "m2", "saldo bca")

	balances, err := f.sources.Balances(f.ctx)
	require.NoError(t, err)
	assert.Equal(t, "BCA: "+money.FormatIDR(balances[f.bca.ID]), f.sender.last())
	assert.Equal(t, 1, len(f.live(t)), "a question writes nothing")
	assert.Equal(t, 0, f.openActions(t))
}

func TestBalanceAllListsEverySource(t *testing.T) {
	script, client := newScriptedLLM(t)
	f := newBotFixture(t, client)

	script.setIntent(`{"intent":"balance","drafts":[],"correction":null,"undo_index":0,"period":"",` +
		`"balance_source_id":null,"answer":null}`)
	f.say(t, "m1", "berapa sisa uang?")

	reply := f.sender.last()
	for _, name := range []string{"BCA", "GoPay", "Cash", "SPayLater", "BRI", "DANA"} {
		assert.Contains(t, reply, name+": ")
	}
	assert.Contains(t, reply, "Safe-to-Spend ")
}

// Without an LLM a message that is not a capture must cost nothing: one
// honest reply, no Hold Queue rows, no writes.
func TestUnknownMessageWithoutLLM(t *testing.T) {
	f := newBotFixture(t, nil)

	f.say(t, "m1", "itu kategori maknan & minuman")

	assert.Empty(t, f.live(t))
	assert.Equal(t, 0, f.openActions(t))
	assert.True(t, strings.HasPrefix(f.sender.last(), "Hmm, aku belum nangkep maksudnya."), "reply was %q", f.sender.last())
}

// The router may point at an amount; it may never invent one. A token
// that is not in the message is treated as no amount at all.
func TestFabricatedAmountIsRefused(t *testing.T) {
	script, client := newScriptedLLM(t)
	f := newBotFixture(t, client)

	script.setIntent(`{"intent":"capture","drafts":[{"type":"expense","amount_text":"50k","merchant":"kopi",` +
		`"source_id":"` + f.cash.ID.String() + `","destination_source_id":null,"category_id":null,"day_offset":0}],` +
		`"correction":null,"undo_index":0,"period":"","balance_source_id":null,"answer":null}`)
	f.say(t, "m1", "ganti kopi 25k")

	assert.Empty(t, f.live(t), "an amount the message never carried is not a write")
	reply := f.sender.last()
	assert.Contains(t, reply, "Aku tahan dulu")
	assert.Contains(t, reply, "Nominalnya berapa?")
}

// The same fixture with the amount actually present writes, and the
// pre-resolved ids are honoured without a second round trip.
func TestIntentCaptureHonoursVerifiedAmount(t *testing.T) {
	script, client := newScriptedLLM(t)
	f := newBotFixture(t, client)

	script.setIntent(`{"intent":"capture","drafts":[{"type":"expense","amount_text":"25k","merchant":"kopi",` +
		`"source_id":"` + f.cash.ID.String() + `","destination_source_id":null,"category_id":"` +
		f.makanan.ID.String() + `","day_offset":-1}],` +
		`"correction":null,"undo_index":0,"period":"","balance_source_id":null,"answer":null}`)
	f.say(t, "m1", "tadi kopi 25k")

	txs := f.live(t)
	require.Len(t, txs, 1)
	assert.Equal(t, int64(25_000), txs[0].AmountMinor)
	assert.Equal(t, f.cash.ID, legSourceID(t, txs[0].Legs))
	require.NotNil(t, txs[0].CategoryID)
	assert.Equal(t, f.makanan.ID, *txs[0].CategoryID)
	require.NotNil(t, txs[0].CategorySource)
	assert.Equal(t, "llm", *txs[0].CategorySource)
	// A pre-resolved category skips the picker entirely.
	assert.Equal(t, 0, script.callsOf("category_pick"))
	assert.Contains(t, f.sender.last(), "Sip, tercatat: Rp25.000")
}

// "dari X" is a Source when it matches the whitelist and the shop
// otherwise, so one message can name both without asking anything — and
// without paying for an alias repair the rules already settled.
func TestPlaceClauseBecomesMerchantAndSourceIsRecognised(t *testing.T) {
	script, client := newScriptedLLM(t)
	f := newBotFixture(t, client)

	f.say(t, "m1", "tissu 20k dari alfamart dari BRI, terus cimol 10k dari DANA")

	txs := f.live(t)
	require.Len(t, txs, 2)
	require.NotNil(t, txs[0].Merchant)
	assert.Equal(t, "alfamart", *txs[0].Merchant)
	require.NotNil(t, txs[0].Note)
	assert.Equal(t, "tissu", *txs[0].Note)
	assert.Equal(t, f.bri.ID, legSourceID(t, txs[0].Legs))
	require.NotNil(t, txs[1].Merchant)
	assert.Equal(t, "cimol", *txs[1].Merchant)
	assert.Nil(t, txs[1].Note)
	assert.Equal(t, f.dana.ID, legSourceID(t, txs[1].Legs))

	reply := f.sender.last()
	assert.Contains(t, reply, "2 transaksi tercatat")
	assert.Contains(t, reply, "alfamart · tissu")
	assert.Equal(t, 0, f.openActions(t))
	assert.Equal(t, 0, script.callsOf("source_pick"), "a from-clause never reaches the model")
}

// "gaji 5jt dari kantor" names a place the whitelist does not know. The
// rules ask instead of guessing, and the model is not consulted.
func TestUnmatchedFromClauseIsAPlaceNotAModelCall(t *testing.T) {
	script, client := newScriptedLLM(t)
	f := newBotFixture(t, client)

	f.say(t, "m1", "gaji 5jt dari kantor")

	assert.Empty(t, f.live(t))
	reply := f.sender.last()
	assert.Contains(t, reply, "Aku tahan dulu: kantor · gaji Rp5.000.000")
	assert.Contains(t, reply, "Bayar pakai sumber mana?")
	assert.Equal(t, 0, script.callsOf("source_pick"))

	open, err := f.pending.ListOpen(f.ctx)
	require.NoError(t, err)
	require.Len(t, open, 1)
	var params map[string]any
	require.NoError(t, json.Unmarshal(open[0].Params, &params))
	assert.Equal(t, "kantor", params["merchant"])
	assert.Equal(t, "gaji", params["note"])
}

// A pakai-clause can only have meant a Source, so a typo is worth
// exactly one repair call.
func TestUnmatchedPayClauseGetsOneRepairCall(t *testing.T) {
	script, client := newScriptedLLM(t)
	f := newBotFixture(t, client)
	script.source = `{"source_id":"` + f.bca.ID.String() + `"}`

	f.say(t, "m1", "kopi 25k pake bcaa")

	txs := f.live(t)
	require.Len(t, txs, 1)
	assert.Equal(t, f.bca.ID, legSourceID(t, txs[0].Legs))
	assert.Equal(t, 1, script.callsOf("source_pick"))
}
