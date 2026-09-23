package bot_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"github.com/glennprays/catet-backend/internal/apperror"
	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/llm"
	"github.com/glennprays/catet-backend/internal/service"
	waga "github.com/glennprays/whatsapp-gateway-sdk-go"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeVisionLLM answers every prompt the photo path can make and counts
// them, so a test can prove a duplicate photo never reaches the model
// and that a rules-answerable reply never reaches the router.
type fakeVisionLLM struct {
	mu             sync.Mutex
	visionCalls    int
	intentCalls    int
	sourceCalls    int
	visionBlock    chan struct{}
	visionCanceled chan struct{}
	receipt        func() string
	sourceID       func() uuid.UUID
	intent         func() string
}
func (f *fakeVisionLLM) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var content string
	switch {
	case bytes.Contains(body, []byte("image_url")):
		f.mu.Lock()
		f.visionCalls++
		gate := f.visionBlock
		f.mu.Unlock()
		if gate != nil {
			select {
			case <-gate:
			case <-r.Context().Done():
				select { case f.visionCanceled <- struct{}{}: default: }
				return
			}
		}
		content = f.receipt()
	case bytes.Contains(body, []byte("(intent)")):
		f.mu.Lock()
		f.intentCalls++
		f.mu.Unlock()
		content = f.intent()
	case bytes.Contains(body, []byte("Alias:")):
		f.mu.Lock()
		f.sourceCalls++
		f.mu.Unlock()
		content = fmt.Sprintf(`{"source_id":%q}`, f.sourceID())
	default:
		content = `{"category_id":null,"confidence":0}`
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"content": content}}},
		"usage":   map[string]any{"prompt_tokens": 1200, "completion_tokens": 400},
	})
}

func (f *fakeVisionLLM) intents() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.intentCalls
}

func (f *fakeVisionLLM) sourcePicks() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sourceCalls
}

// unknownIntent is the router saying it has no idea: the default script

func (f *fakeVisionLLM) blockVision() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.visionBlock = make(chan struct{})
}
// for a test that does not care what it says.
const unknownIntent = `{"intent":"unknown","drafts":[],"correction":null,"undo_index":0,` +
	`"period":"","balance_source_id":null,"answer":null}`

func (f *fakeVisionLLM) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.visionCalls
}

// jpegBytes is a real (tiny) JPEG: the media fetch sniffs the body, so a
// fixture has to be a decodable image.
func jpegBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: 200, G: 200, B: 200, A: 255})
	var buf bytes.Buffer
	require.NoError(t, jpeg.Encode(&buf, img, nil))
	return buf.Bytes()
}

// mediaServer stands in for the gateway's /storage endpoint.
func mediaServer(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// sendPhoto delivers one image message with the gateway's media shape.
func (f *botFixture) sendPhoto(t *testing.T, messageID, url string, size int, caption string) {
	t.Helper()
	f.deliver(t, &waga.IncomingWebhookPayload{
		Event:     waga.IncomingEventMessageMessageIncoming,
		Chat:      ownerPhone[1:] + "@s.whatsapp.net",
		From:      ownerPhone[1:] + ":42@s.whatsapp.net",
		MessageId: messageID,
		PushName:  "Owner",
		Timestamp: time.Now().Unix(),
		Type:      waga.IncomingMessageTypeImage,
		Media: &waga.IncomingMessageMediaInfo{
			Type: waga.IncomingMessageTypeImage, Url: url,
			MimeType: "image/jpeg", Size: size, Caption: caption,
		},
	})
}

// newPhotoFixture wires a bot whose LLM is the fake vision endpoint and
// whose media comes from a local HTTP server.
func newPhotoFixture(t *testing.T, receipt func(*botFixture) string) (*botFixture, *fakeVisionLLM, string, int) {
	return newPhotoFixtureWithTimeout(t, receipt, 5*time.Second)
}

func newPhotoFixtureWithTimeout(
	t *testing.T, receipt func(*botFixture) string, timeout time.Duration,
) (*botFixture, *fakeVisionLLM, string, int) {
	t.Helper()
	img := jpegBytes(t)
	media := mediaServer(t, img)

	fake := &fakeVisionLLM{visionCanceled: make(chan struct{}, 2)}
	llmSrv := httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(llmSrv.Close)

	f := newBotFixture(t, llm.New(llmSrv.URL, "test-key", "test-model", timeout))
	fake.receipt = func() string { return receipt(f) }
	fake.sourceID = func() uuid.UUID { return f.bca.ID }
	fake.intent = func() string { return unknownIntent }
	return f, fake, media.URL + "/storage/x.jpg", len(img)
}

func TestPhotoVisionExhaustionStoresRejectedReplyWithCoreTail(t *testing.T) {
	f, fake, url, size := newPhotoFixtureWithTimeout(t, func(*botFixture) string {
		return `{"document_type":"receipt","legible":true,"issues":[],"merchant":"Indomaret","date":"","currency":"IDR","has_total_line":true,"total_minor":1000,"subtotal_minor":1000,"discount_minor":0,"tax_minor":0,"rounding_minor":0,"payment_method":"cash","direction":"","counterparty":"","items":[]}`
	}, 2*time.Second)
	fake.blockVision()
	ctx, cancel := context.WithTimeout(f.ctx, 24*time.Second)
	defer cancel()

	err := f.harness.Process(ctx, &waga.IncomingWebhookPayload{
		Event: waga.IncomingEventMessageMessageIncoming,
		Chat: ownerPhone[1:] + "@s.whatsapp.net",
		From: ownerPhone[1:] + ":42@s.whatsapp.net",
		MessageId: "m-vision-timeout",
		PushName: "Owner",
		Timestamp: time.Now().Unix(),
		Type: waga.IncomingMessageTypeImage,
		Media: &waga.IncomingMessageMediaInfo{
			Type: waga.IncomingMessageTypeImage, Url: url,
			MimeType: "image/jpeg", Size: size,
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 2, fake.calls(), "vision provider is exhausted through both attempts")
	_, ok := <-fake.visionCanceled
	assert.True(t, ok, "vision request observes core-work cancellation before finalization")
	_, ok = <-fake.visionCanceled
	assert.True(t, ok, "vision retry also observes cancellation")
	var status string
	var reply, lastError *string
	require.NoError(t, f.pool.QueryRow(context.Background(),
		`SELECT status, reply_text, last_error FROM inbound_messages WHERE message_id = $1`,
		"m-vision-timeout").Scan(&status, &reply, &lastError))
	assert.Equal(t, "replied", status)
	require.NotNil(t, reply, "finalization must persist the user-facing error reply")
	assert.NotEmpty(t, *reply)
	assert.NotEqual(t, "👀", *reply)
	row, err := f.inbound.Get(f.ctx, "m-vision-timeout")
	require.NoError(t, err)
	require.NotNil(t, row)
	require.NotNil(t, row.MediaSha256, "downloaded media is fingerprinted before vision")
	assert.Len(t, *row.MediaSha256, 64)
	require.NotNil(t, row.OutcomeReaction)
	assert.Equal(t, "❌", *row.OutcomeReaction)
	assert.Equal(t, []string{"👀", "❌"}, f.sender.reactionsOn("m-vision-timeout"))
	assert.Equal(t, *reply, f.sender.last())
}

// twoLineReceipt is the canonical reading: two items that add up to the
// printed total, paid with an alias only the model can resolve.
func twoLineReceipt(f *botFixture) string {
	return fmt.Sprintf(`{
		"document_type":"receipt","legible":true,"issues":[],
		"merchant":"Indomaret","date":"","currency":"IDR",
		"has_total_line":true,"total_minor":35000,
		"subtotal_minor":35000,"discount_minor":0,"tax_minor":0,"rounding_minor":0,
		"payment_method":"DEBIT BCA","direction":"","counterparty":"",
		"items":[
			{"name":"Indomie goreng","qty":2,"amount_minor":20000,"category_id":%q,"category_confidence":0.9},
			{"name":"Aqua 600ml","qty":1,"amount_minor":15000,"category_id":null,"category_confidence":0}
		]
	}`, f.makanan.ID)
}

func TestPhotoParksReceiptThenYaWritesLines(t *testing.T) {
	f, fake, url, size := newPhotoFixture(t, twoLineReceipt)

	f.sendPhoto(t, "m1", url, size, "")

	// A photo never writes on arrival: it parks and asks.
	assert.Empty(t, f.live(t), "a photo writes nothing before confirmation")
	open, err := f.pending.ListOpen(f.ctx)
	require.NoError(t, err)
	require.Len(t, open, 1)
	assert.Equal(t, "receipt", open[0].Kind)

	var params map[string]any
	require.NoError(t, json.Unmarshal(open[0].Params, &params))
	assert.Equal(t, float64(35000), params["total_minor"])
	assert.Equal(t, "Indomaret", params["merchant"])
	assert.Equal(t, "BCA", params["source_name"], "the printed payment method resolved the Source")
	items, ok := params["items"].([]any)
	require.True(t, ok)
	require.Len(t, items, 2)

	reply := f.sender.last()
	assert.Contains(t, reply, "Struk Indomaret")
	assert.Contains(t, reply, "Indomie goreng")
	assert.Contains(t, reply, `Balas "ya"`)
	assert.Equal(t, 1, fake.calls())

	// The media metadata is recorded; the bytes are not.
	row, err := f.inbound.Get(f.ctx, "m1")
	require.NoError(t, err)
	require.NotNil(t, row)
	require.NotNil(t, row.MediaURL)
	assert.Equal(t, url, *row.MediaURL)
	require.NotNil(t, row.MediaSha256)
	assert.Len(t, *row.MediaSha256, 64)

	f.say(t, "m2", "ya")

	txs := f.live(t)
	require.Len(t, txs, 2, "one ledger line per receipt item")
	hashes := map[string]bool{}
	for i, tx := range txs {
		assert.Equal(t, "whatsapp_photo", tx.Origin)
		assert.Equal(t, "expense", tx.Type)
		require.NotNil(t, tx.ExternalID)
		assert.Equal(t, fmt.Sprintf("m1#%d", i+1), *tx.ExternalID)
		require.NotNil(t, tx.Merchant)
		assert.Equal(t, "Indomaret", *tx.Merchant)
		assert.Equal(t, f.bca.ID, legSourceID(t, tx.Legs))
		var hash *string
		require.NoError(t, f.pool.QueryRow(f.ctx,
			`SELECT content_hash FROM transactions WHERE id = $1`, tx.ID).Scan(&hash))
		require.NotNil(t, hash)
		hashes[*hash] = true
	}
	assert.Len(t, hashes, 1, "every line of one receipt shares its fingerprint")
	assert.Equal(t, int64(20_000), txs[0].AmountMinor)
	assert.Equal(t, int64(15_000), txs[1].AmountMinor)
	require.NotNil(t, txs[0].Note)
	assert.Equal(t, "Indomie goreng", *txs[0].Note)
	require.NotNil(t, txs[0].CategoryID)
	assert.Equal(t, f.makanan.ID, *txs[0].CategoryID)
	assert.Equal(t, "clean", txs[0].ReviewStatus)
	assert.Nil(t, txs[1].CategoryID)
	assert.Equal(t, "needs_review", txs[1].ReviewStatus, "an unlabelled line needs a human")

	assert.Contains(t, f.sender.last(), "2 baris dari Indomaret tercatat")
	open, err = f.pending.ListOpen(f.ctx)
	require.NoError(t, err)
	assert.Empty(t, open, "the parked receipt is resolved once executed")

	// "batal" reverses the whole batch the confirmation wrote.
	f.say(t, "m3", "batal")
	for _, tx := range txs {
		var reversals int
		require.NoError(t, f.pool.QueryRow(f.ctx,
			`SELECT COUNT(*) FROM transactions WHERE reverses_transaction_id = $1`, tx.ID,
		).Scan(&reversals))
		assert.Equal(t, 1, reversals, "line %s was not reversed", tx.ID)
	}
}

func TestPhotoFailedReplyStaysReadyAfterParkingReceipt(t *testing.T) {
	f, _, url, size := newPhotoFixture(t, twoLineReceipt)
	f.sender.setSendError(errors.New("gateway unavailable"))

	f.sendPhoto(t, "m1", url, size, "")

	assert.Empty(t, f.live(t), "receipt arrival only parks a Hold Queue draft")
	open, err := f.pending.ListOpen(f.ctx)
	require.NoError(t, err)
	require.Len(t, open, 1)
	assert.Equal(t, "receipt", open[0].Kind)

	row, err := f.inbound.Get(f.ctx, "m1")
	require.NoError(t, err)
	require.NotNil(t, row)
	require.Equal(t, "ready", row.Status)
	require.NotNil(t, row.ReplyText)
	assert.Empty(t, row.TransactionIDs)
	assert.Empty(t, f.sender.all(), "a failed receipt reply was not accepted")

	f.sender.setSendError(nil)
	f.sendPhoto(t, "m1", url, size, "")
	assert.Empty(t, f.sender.all(), "a not-yet-due ready delivery cannot be retried immediately")
	require.NoError(t, f.pool.QueryRow(f.ctx,
		`UPDATE inbound_messages SET next_attempt_at = now() - interval '1 second' WHERE message_id = $1 RETURNING message_id`,
		"m1",
	).Scan(new(string)))
	f.sendPhoto(t, "m1", url, size, "")

	assert.Empty(t, f.live(t), "receipt delivery recovery never writes ledger money")
	open, err = f.pending.ListOpen(f.ctx)
	require.NoError(t, err)
	require.Len(t, open, 1, "delivery recovery never creates a second Hold Queue draft")
	sent := f.sender.all()
	require.Len(t, sent, 1)
	assert.Equal(t, *row.ReplyText, sent[0].Text)
	assert.Equal(t, "reply:m1", sent[0].IdempotencyKey)
}


// A receipt Hold is committed before the inbound reply result. If result
// storage fails, retrying the same delivery must recover the original Hold
// rather than creating a second dashboard action.
func TestPhotoResultStoreFailureReusesReceiptHold(t *testing.T) {
	f, _, url, size := newPhotoFixture(t, twoLineReceipt)
	_, err := f.pool.Exec(f.ctx, `
		CREATE FUNCTION test_fail_receipt_result_store() RETURNS trigger
		LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.status = 'ready' AND OLD.status = 'processing' THEN
				RAISE EXCEPTION 'forced receipt result-store failure';
			END IF;
			RETURN NEW;
		END
		$$;
		CREATE TRIGGER test_fail_receipt_result_store_trigger
		BEFORE UPDATE ON inbound_messages
		FOR EACH ROW EXECUTE FUNCTION test_fail_receipt_result_store();
	`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `
			DROP TRIGGER IF EXISTS test_fail_receipt_result_store_trigger ON inbound_messages;
			DROP FUNCTION IF EXISTS test_fail_receipt_result_store();
		`)
	})

	f.sendPhoto(t, "m-receipt-crash", url, size, "")

	open, err := f.pending.ListOpen(f.ctx)
	require.NoError(t, err)
	require.Len(t, open, 1, "the Hold is committed before result storage")
	first := open[0]
	var params map[string]any
	require.NoError(t, json.Unmarshal(first.Params, &params))
	assert.Equal(t, "m-receipt-crash#receipt", params["external_id"])
	row, err := f.inbound.Get(f.ctx, "m-receipt-crash")
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, "processing", row.Status)
	assert.Empty(t, f.sender.all(), "result-store failure sends no reply")

	_, err = f.pool.Exec(f.ctx, `
		DROP TRIGGER IF EXISTS test_fail_receipt_result_store_trigger ON inbound_messages;
		DROP FUNCTION IF EXISTS test_fail_receipt_result_store();
		UPDATE inbound_messages
		SET next_attempt_at = now() - interval '1 second'
		WHERE message_id = 'm-receipt-crash'
	`)
	require.NoError(t, err)

	f.sendPhoto(t, "m-receipt-crash", url, size, "")

	open, err = f.pending.ListOpen(f.ctx)
	require.NoError(t, err)
	require.Len(t, open, 1, "retry reuses the receipt Hold")
	assert.Equal(t, first.ID, open[0].ID)
	assert.Empty(t, f.live(t), "receipt retries never write ledger money")
	sent := f.sender.all()
	require.Len(t, sent, 1)
	assert.Equal(t, "reply:m-receipt-crash", sent[0].IdempotencyKey)
}

func TestPhotoRetryDoesNotReopenTerminalReceiptHold(t *testing.T) {
	f, _, url, size := newPhotoFixture(t, twoLineReceipt)
	f.sendPhoto(t, "m-receipt-terminal", url, size, "")

	open, err := f.pending.ListOpen(f.ctx)
	require.NoError(t, err)
	require.Len(t, open, 1)
	_, err = f.pending.Resolve(f.ctx, open[0].ID, "discard")
	require.NoError(t, err)
	_, err = f.pool.Exec(f.ctx, `
		UPDATE bot_chat_state
		SET open_pending_action_id = NULL, open_expect = NULL
		WHERE chat = $1
	`, ownerPhone[1:]+"@s.whatsapp.net")
	require.NoError(t, err)
	_, err = f.pool.Exec(f.ctx, `
		UPDATE inbound_messages
		SET status = 'processing', reply_text = NULL, outcome_reaction = NULL,
		    next_attempt_at = now() - interval '1 second', lease_until = NULL
		WHERE message_id = 'm-receipt-terminal'
	`)
	require.NoError(t, err)

	f.sendPhoto(t, "m-receipt-terminal", url, size, "")

	open, err = f.pending.ListOpen(f.ctx)
	require.NoError(t, err)
	assert.Empty(t, open, "a terminal receipt Hold is not reopened")
	assert.Empty(t, f.live(t), "receipt retries never write ledger money")
	assert.Len(t, f.sender.all(), 2)
}
func TestPhotoSameBytesTwiceIsNotReparsed(t *testing.T) {
	f, fake, url, size := newPhotoFixture(t, twoLineReceipt)

	f.sendPhoto(t, "m1", url, size, "")
	require.Equal(t, 1, fake.calls())
	first := f.sender.last()

	// The same picture forwarded again, under a new message id.
	f.sendPhoto(t, "m2", url, size, "")

	assert.Equal(t, 1, fake.calls(), "the same bytes never reach the model twice")
	assert.True(t, strings.HasPrefix(f.sender.last(), "Foto ini sudah pernah kamu kirim"),
		"reply was %q", f.sender.last())
	assert.Contains(t, f.sender.last(), "Struk Indomaret")

	open, err := f.pending.ListOpen(f.ctx)
	require.NoError(t, err)
	assert.Len(t, open, 1, "a duplicate photo parks nothing new")
	assert.NotEqual(t, first, f.sender.last())
}

// A fetched image is fingerprinted even when vision rejects it. A forwarded
// copy then gets the durable duplicate refusal without a second vision call.
func TestPhotoFailedReadIsReplayedAsDuplicate(t *testing.T) {
	f, fake, url, size := newPhotoFixture(t, twoLineReceipt)
	fake.receipt = func() string { return "upstream hiccup" }
	f.sendPhoto(t, "m1", url, size, "")

	assert.True(t, strings.HasPrefix(f.sender.last(), "Aduh, ada yang error"),
		"reply was %q", f.sender.last())
	row, err := f.inbound.Get(f.ctx, "m1")
	require.NoError(t, err)
	require.NotNil(t, row)
	require.NotNil(t, row.MediaSha256, "a fetched image is fingerprinted even when vision rejects it")
	assert.Len(t, *row.MediaSha256, 64)

	fake.receipt = func() string { return twoLineReceipt(f) }
	f.sendPhoto(t, "m2", url, size, "")

	assert.Equal(t, 1, fake.calls(), "a rejected image is not sent to vision twice")
	assert.True(t, strings.HasPrefix(f.sender.last(), "Foto ini sudah pernah kamu kirim"),
		"reply was %q", f.sender.last())
	open, err := f.pending.ListOpen(f.ctx)
	require.NoError(t, err)
	assert.Empty(t, open, "a duplicate refusal never parks a draft")
}

// A gateway that does not download media hands us WhatsApp's encrypted
// CDN body. It must never reach the model as if it were a photo.
func TestPhotoCiphertextIsUnreadable(t *testing.T) {
	media := mediaServer(t, bytes.Repeat([]byte{0x9f, 0x01, 0xff, 0x00}, 16))

	fake := &fakeVisionLLM{
		receipt:  func() string { return `{}` },
		sourceID: uuid.New,
	}
	llmSrv := httptest.NewServer(http.HandlerFunc(fake.serve))
	defer llmSrv.Close()

	f := newBotFixture(t, llm.New(llmSrv.URL, "test-key", "test-model", 5*time.Second))
	f.sendPhoto(t, "m1", media.URL+"/storage/x.enc", 64, "")

	assert.Equal(t, 0, fake.calls(), "undecryptable bytes never reach the model")
	assert.Contains(t, f.sender.last(), "Fotonya")
	assert.Empty(t, f.live(t))
	open, err := f.pending.ListOpen(f.ctx)
	require.NoError(t, err)
	assert.Empty(t, open)
	row, err := f.inbound.Get(f.ctx, "m1")
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, "replied", row.Status)
	assert.Nil(t, row.MediaSha256, "rejected bytes were never accepted or fingerprinted")
	require.NotNil(t, row.OutcomeReaction)
	assert.Equal(t, "❌", *row.OutcomeReaction)
}

// Without a vision model the bot says so instead of guessing.
func TestPhotoWithoutLlmSaysSo(t *testing.T) {
	img := jpegBytes(t)
	media := mediaServer(t, img)

	f := newBotFixture(t, nil)
	f.sendPhoto(t, "m1", media.URL+"/storage/x.jpg", len(img), "")

	assert.Contains(t, f.sender.last(), "Baca struk lagi nonaktif")
	assert.Empty(t, f.live(t))
}

// A receipt whose printed total was already typed in by hand the same
// day is parked with a duplicate warning — not refused, not merged.
func TestPhotoOfAnAlreadyCapturedExpenseWarnsAboutDuplication(t *testing.T) {
	f, _, url, size := newPhotoFixture(t, func(f *botFixture) string {
		return fmt.Sprintf(`{
			"document_type":"receipt","legible":true,"issues":[],
			"merchant":"Indomaret","date":"","currency":"IDR",
			"has_total_line":true,"total_minor":35000,
			"subtotal_minor":35000,"discount_minor":0,"tax_minor":0,"rounding_minor":0,
			"payment_method":"BCA","direction":"","counterparty":"",
			"items":[{"name":"Indomie goreng","qty":1,"amount_minor":35000,"category_id":%q,"category_confidence":0.9}]
		}`, f.makanan.ID)
	})

	f.say(t, "m1", "indomaret 35rb pakai bca")
	require.Len(t, f.live(t), 1)

	f.sendPhoto(t, "m2", url, size, "")

	open, err := f.pending.ListOpen(f.ctx)
	require.NoError(t, err)
	require.Len(t, open, 1)
	var params map[string]any
	require.NoError(t, json.Unmarshal(open[0].Params, &params))
	assert.NotEmpty(t, params["duplicate_of"], "the earlier capture is flagged on the draft")
	assert.Contains(t, open[0].Summary, "Mungkin duplikat")
	assert.Contains(t, f.sender.last(), "Mungkin duplikat")
}

// cashReceipt is the trap the live test fell into: the paper says TUNAI
// because that is what a register prints, while the money actually left
// a bank account. The bot must ask rather than file it under Cash.
func cashReceipt(merchant string, f *botFixture) string {
	return fmt.Sprintf(`{
		"document_type":"receipt","legible":true,"issues":[],
		"merchant":%q,"date":"","currency":"IDR",
		"has_total_line":true,"total_minor":35000,
		"subtotal_minor":35000,"discount_minor":0,"tax_minor":0,"rounding_minor":0,
		"payment_method":"TUNAI","direction":"","counterparty":"",
		"items":[
			{"name":"Indomie goreng","qty":2,"amount_minor":20000,"category_id":%q,"category_confidence":0.9},
			{"name":"Aqua 600ml","qty":1,"amount_minor":15000,"category_id":null,"category_confidence":0}
		]
	}`, merchant, f.makanan.ID)
}

// photoDefaultSource reads the learned fallback from the owner's
// settings row.
func (f *botFixture) photoDefaultSource(t *testing.T) *uuid.UUID {
	t.Helper()
	id, err := f.settings.GetPhotoDefaultSource(f.ctx)
	require.NoError(t, err)
	return id
}

func (f *botFixture) openQuestion(t *testing.T) *uuid.UUID {
	t.Helper()
	state, err := f.chats.Get(f.ctx, ownerPhone[1:]+"@s.whatsapp.net")
	require.NoError(t, err)
	if state == nil {
		return nil
	}
	return state.OpenPendingActionID
}

// The round-2 headline: printed cash decides nothing, and one message
// may both name the Source and confirm the draft.
func TestPhotoAnswerWithSourceAndYaWritesFromThatSource(t *testing.T) {
	f, fake, url, size := newPhotoFixture(t, func(f *botFixture) string {
		return cashReceipt("Indomaret", f)
	})

	f.sendPhoto(t, "m1", url, size, "")

	reply := f.sender.last()
	assert.Contains(t, reply, "Bayar dari mana?", "printed TUNAI must not decide, reply was %q", reply)
	assert.Equal(t, 0, fake.sourcePicks(), "a cash-like method never pays for an alias repair")
	assert.Empty(t, f.live(t))

	f.say(t, "m2", "bayar pake BCA, ya catat")

	txs := f.live(t)
	require.Len(t, txs, 2, "one ledger line per receipt item")
	for _, tx := range txs {
		assert.Equal(t, f.bca.ID, legSourceID(t, tx.Legs))
	}
	reply = f.sender.last()
	assert.Contains(t, reply, "2 baris dari Indomaret tercatat")
	assert.Contains(t, reply, "Struk berikutnya kuanggap dari BCA")

	id := f.photoDefaultSource(t)
	require.NotNil(t, id, "the first Source the user picks becomes the fallback")
	assert.Equal(t, f.bca.ID, *id)

	assert.Equal(t, 1, fake.calls(), "one vision call")
	assert.Equal(t, 0, fake.intents(), "a message the rules explain never reaches the router")
	assert.Equal(t, []string{"👀", "📋", "✅"}, f.sender.reactionsOn("m1"))
	assert.Nil(t, f.openQuestion(t))
}

// A message the bot cannot use must never cost the user their draft.
func TestPhotoUnknownReplyKeepsQuestionOpen(t *testing.T) {
	f, _, url, size := newPhotoFixture(t, twoLineReceipt)

	f.sendPhoto(t, "m1", url, size, "")
	open := f.openQuestion(t)
	require.NotNil(t, open)

	f.say(t, "m2", "halo apa kabar")

	assert.True(t, strings.HasPrefix(f.sender.last(), "Aku masih nunggu"),
		"reply was %q", f.sender.last())
	still := f.openQuestion(t)
	require.NotNil(t, still, "the question survives a message that was not an answer")
	assert.Equal(t, *open, *still)
	assert.Empty(t, f.live(t))
}

// One bill, two Sources: the shares are money facts, so the lines bend
// around them and the split still sums to the printed total.
func TestPhotoSplitPaymentWritesBothSources(t *testing.T) {
	f, fake, url, size := newPhotoFixture(t, func(f *botFixture) string {
		return fmt.Sprintf(`{
			"document_type":"receipt","legible":true,"issues":[],
			"merchant":"Indomaret","date":"","currency":"IDR",
			"has_total_line":true,"total_minor":38000,
			"subtotal_minor":38000,"discount_minor":0,"tax_minor":0,"rounding_minor":0,
			"payment_method":"TUNAI","direction":"","counterparty":"",
			"items":[
				{"name":"a","qty":1,"amount_minor":10000,"category_id":%q,"category_confidence":0.9},
				{"name":"b","qty":1,"amount_minor":9000,"category_id":null,"category_confidence":0},
				{"name":"c","qty":1,"amount_minor":8000,"category_id":null,"category_confidence":0},
				{"name":"d","qty":1,"amount_minor":5000,"category_id":null,"category_confidence":0},
				{"name":"e","qty":1,"amount_minor":4000,"category_id":null,"category_confidence":0},
				{"name":"f","qty":1,"amount_minor":2000,"category_id":null,"category_confidence":0}
			]
		}`, f.makanan.ID)
	})

	f.sendPhoto(t, "m1", url, size, "")
	require.Contains(t, f.sender.last(), "Bayar dari mana?")

	fake.intent = func() string {
		return fmt.Sprintf(`{"intent":"answer","drafts":[],"correction":null,"undo_index":0,`+
			`"period":"","balance_source_id":null,"answer":{"confirm":"yes","source_id":null,`+
			`"payments":[{"source_id":%q,"amount_text":"20rb"},{"source_id":%q,"amount_text":""}],`+
			`"category_id":null,"merchant":null,"day_offset":null}}`, f.cash.ID, f.bca.ID)
	}
	f.say(t, "m2", "20rb cash sisanya bca")

	perSource := map[uuid.UUID]int64{}
	externals := map[string]bool{}
	var total int64
	for _, tx := range f.live(t) {
		perSource[legSourceID(t, tx.Legs)] += tx.AmountMinor
		total += tx.AmountMinor
		require.NotNil(t, tx.ExternalID)
		externals[*tx.ExternalID] = true
	}
	assert.Equal(t, int64(38_000), total, "the split still sums to the printed total")
	assert.Equal(t, int64(20_000), perSource[f.cash.ID])
	assert.Equal(t, int64(18_000), perSource[f.bca.ID])
	assert.True(t, externals["m1#3a"], "the cut line's halves keep their own idempotency keys")
	assert.True(t, externals["m1#3b"])
	assert.Contains(t, f.sender.last(), "7 baris dari Indomaret tercatat", "the straddling line is written as two")
}

// The Source memory answers what the paper cannot: this shop was paid
// from BCA last time, so say so instead of asking again.
func TestPhotoMerchantMemoryPicksSource(t *testing.T) {
	f, fake, url, size := newPhotoFixture(t, func(f *botFixture) string {
		return cashReceipt("Alfamart", f)
	})

	f.say(t, "m0", "alfamart 20k pakai bca")
	require.Len(t, f.live(t), 1)

	f.sendPhoto(t, "m1", url, size, "")

	reply := f.sender.last()
	assert.Contains(t, reply, "Bayar dari BCA (biasanya).", "reply was %q", reply)
	assert.Equal(t, 0, fake.sourcePicks())
}

// The fallback is learned once. Overwriting it on every answer would
// make the bot's "usually" wander with the last receipt.
func TestPhotoDefaultLearnedOnlyOnce(t *testing.T) {
	f, _, url, size := newPhotoFixture(t, func(f *botFixture) string {
		return cashReceipt("Indomaret", f)
	})

	f.sendPhoto(t, "m1", url, size, "")
	f.say(t, "m2", "pakai bca ya")
	first := f.photoDefaultSource(t)
	require.NotNil(t, first)
	require.Equal(t, f.bca.ID, *first)

	// A second receipt, answered with a different Source.
	img := jpegBytes(t)
	other := mediaServer(t, append(img, 0x00))
	f.sendPhoto(t, "m3", other.URL+"/storage/y.jpg", len(img)+1, "")
	f.say(t, "m4", "pakai gopay ya")

	assert.NotContains(t, f.sender.last(), "Struk berikutnya",
		"the fallback is only ever announced when it is learned")
	after := f.photoDefaultSource(t)
	require.NotNil(t, after)
	assert.Equal(t, f.bca.ID, *after, "the learned fallback is not overwritten")
}

// The caption is the user speaking, so it outranks whatever the paper
// printed — and it resolves with the rules alone when it is plainly a
// Source name.
func TestPhotoCaptionBeatsThePrintedMethod(t *testing.T) {
	f, fake, url, size := newPhotoFixture(t, twoLineReceipt) // prints "DEBIT BCA"

	f.sendPhoto(t, "m1", url, size, "gopay")

	open, err := f.pending.ListOpen(f.ctx)
	require.NoError(t, err)
	require.Len(t, open, 1)
	var params map[string]any
	require.NoError(t, json.Unmarshal(open[0].Params, &params))
	assert.Equal(t, "GoPay", params["source_name"])
	assert.Equal(t, "hint", params["source_basis"])
	assert.Contains(t, f.sender.last(), "Bayar dari GoPay.")
	assert.Equal(t, 0, fake.intents(), "a caption that is a Source name needs no router")
}

// A caption the rules cannot read is worth one router call, which may
// label every line of the receipt at once.
func TestPhotoCaptionCategoryLabelsEveryLine(t *testing.T) {
	f, fake, url, size := newPhotoFixture(t, twoLineReceipt)
	fake.intent = func() string {
		return fmt.Sprintf(`{"intent":"answer","drafts":[],"correction":null,"undo_index":0,`+
			`"period":"","balance_source_id":null,"answer":{"confirm":"","source_id":null,"payments":[],`+
			`"category_id":%q,"merchant":null,"day_offset":null}}`, f.transpor.ID)
	}

	f.sendPhoto(t, "m1", url, size, "ini buat jalan-jalan kemarin")

	assert.Equal(t, 1, fake.intents())
	f.say(t, "m2", "ya")

	txs := f.live(t)
	require.Len(t, txs, 2)
	for _, tx := range txs {
		require.NotNil(t, tx.CategoryID)
		assert.Equal(t, f.transpor.ID, *tx.CategoryID)
		require.NotNil(t, tx.CategorySource)
		assert.Equal(t, "user", *tx.CategorySource, "a label the user gave is not a guess")
		assert.Equal(t, "clean", tx.ReviewStatus)
	}
}

// A vision model that reports it never received the image is our outage,
// and the reply says so instead of blaming the photo.
func TestPhotoVisionOutageIsNotCalledNotAReceipt(t *testing.T) {
	f, fake, url, size := newPhotoFixture(t, func(*botFixture) string {
		return `{"image_received":false,"document_type":"other","legible":false,"issues":[],` +
			`"merchant":"","date":"","currency":"","has_total_line":false,"total_minor":0,` +
			`"subtotal_minor":0,"discount_minor":0,"tax_minor":0,"rounding_minor":0,` +
			`"payment_method":"","direction":"","counterparty":"","items":[]}`
	})

	f.sendPhoto(t, "m1", url, size, "")


	assert.True(t, strings.HasPrefix(f.sender.last(), "Aku lagi nggak bisa lihat foto"),
		"reply was %q", f.sender.last())
	assert.Equal(t, 1, fake.calls())
	open, err := f.pending.ListOpen(f.ctx)
	require.NoError(t, err)
	assert.Empty(t, open, "an outage parks nothing")
	assert.Empty(t, f.live(t))
}

func TestPhotoNoTotalStoresRejectedOutcomeWithoutMoney(t *testing.T) {
	f, fake, url, size := newPhotoFixture(t, func(*botFixture) string {
		return `{"image_received":true,"document_type":"receipt","legible":true,"issues":[],
			"merchant":"Indomaret","date":"","currency":"IDR","has_total_line":false,
			"total_minor":0,"subtotal_minor":1000,"discount_minor":0,"tax_minor":0,
			"rounding_minor":0,"payment_method":"cash","direction":"","counterparty":"","items":[]}`
	})

	f.sendPhoto(t, "m-no-total", url, size, "")

	assert.Equal(t, 1, fake.calls())
	assert.Contains(t, f.sender.last(), "total")
	assert.Empty(t, f.live(t))
	open, err := f.pending.ListOpen(f.ctx)
	require.NoError(t, err)
	assert.Empty(t, open)
	row, err := f.inbound.Get(f.ctx, "m-no-total")
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, "replied", row.Status)
	require.NotNil(t, row.MediaSha256)
	require.NotNil(t, row.OutcomeReaction)
	assert.Equal(t, "❌", *row.OutcomeReaction)
}

func TestPhotoHashPersistenceFailureRefusesBeforeVision(t *testing.T) {
	f, fake, url, size := newPhotoFixture(t, twoLineReceipt)
	_, err := f.pool.Exec(f.ctx, `
		CREATE FUNCTION test_fail_media_hash() RETURNS trigger
		LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.media_sha256 IS DISTINCT FROM OLD.media_sha256
				AND NEW.media_sha256 IS NOT NULL THEN
				RAISE EXCEPTION 'forced media hash failure';
			END IF;
			RETURN NEW;
		END
		$$;
		CREATE TRIGGER test_fail_media_hash_trigger
		BEFORE UPDATE ON inbound_messages
		FOR EACH ROW EXECUTE FUNCTION test_fail_media_hash();
	`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `
			DROP TRIGGER IF EXISTS test_fail_media_hash_trigger ON inbound_messages;
			DROP FUNCTION IF EXISTS test_fail_media_hash();
		`)
	})

	f.sendPhoto(t, "m-hash-failure", url, size, "")

	assert.Equal(t, 0, fake.calls(), "vision must not run before hash persistence")
	assert.Empty(t, f.live(t))
	open, err := f.pending.ListOpen(f.ctx)
	require.NoError(t, err)
	assert.Empty(t, open)
	row, err := f.inbound.Get(f.ctx, "m-hash-failure")
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, "replied", row.Status)
	assert.Nil(t, row.MediaSha256)
	require.NotNil(t, row.ReplyText)
	assert.NotEqual(t, "👀", *row.ReplyText)
	require.NotNil(t, row.OutcomeReaction)
	assert.Equal(t, "❌", *row.OutcomeReaction)
}

// ------------------------------------------------ dashboard confirmation

// ptr is the one-liner the override structs need: every correction is a
// pointer, because "absent" and "cleared" are different answers.
func ptr[T any](v T) *T { return &v }

// parkedReceipt sends the cash receipt (2 lines, no Source the bot may
// trust) and returns the parked draft the dashboard would confirm.
func parkedReceipt(t *testing.T) (*botFixture, entity.PendingAction) {
	t.Helper()
	f, _, url, size := newPhotoFixture(t, func(f *botFixture) string {
		return cashReceipt("Indomaret", f)
	})
	f.sendPhoto(t, "m1", url, size, "")
	open, err := f.pending.ListOpen(f.ctx)
	require.NoError(t, err)
	require.Len(t, open, 1)
	require.Equal(t, "receipt", open[0].Kind)
	return f, open[0]
}

// detailOf is how a bot test reads a field rule off a validation error.
func detailOf(t *testing.T, err error) apperror.FieldError {
	t.Helper()
	var appErr *apperror.Error
	require.True(t, errors.As(err, &appErr), "want an apperror, got %v", err)
	require.Len(t, appErr.Details, 1)
	return appErr.Details[0]
}

// The dashboard's Konfirmasi: it answers the Source question the chat
// asked, and may relabel a single line without touching the others.
func TestDashboardConfirmPicksSourceAndPerLineCategory(t *testing.T) {
	f, hold := parkedReceipt(t)

	txs, err := f.harness.Execute(f.ctx, &hold, &service.PendingOverrides{
		SourceID: &f.gopay.ID,
		Items:    []service.PendingItemOverride{{N: 2, CategoryID: &f.transpor.ID}},
	})
	require.NoError(t, err)
	require.Len(t, txs, 2, "one ledger line per receipt item")

	live := f.live(t)
	require.Len(t, live, 2)
	for _, tx := range live {
		assert.Equal(t, f.gopay.ID, legSourceID(t, tx.Legs), "the picked Source paid every line")
	}
	require.NotNil(t, live[0].CategoryID)
	assert.Equal(t, f.makanan.ID, *live[0].CategoryID, "an untouched line keeps the model's label")
	require.NotNil(t, live[0].CategorySource)
	assert.Equal(t, "llm", *live[0].CategorySource)
	require.NotNil(t, live[1].CategoryID)
	assert.Equal(t, f.transpor.ID, *live[1].CategoryID)
	require.NotNil(t, live[1].CategorySource)
	assert.Equal(t, "user", *live[1].CategorySource, "a label the user picked is not a guess")
	assert.Equal(t, "clean", live[1].ReviewStatus)

	id := f.photoDefaultSource(t)
	require.NotNil(t, id, "a Source picked in the dashboard teaches the fallback too")
	assert.Equal(t, f.gopay.ID, *id)
}

// The opt-out: one line at the printed total, under an external_id no
// per-line attempt can ever collide with.
func TestDashboardConfirmCollapsesToOneLine(t *testing.T) {
	f, hold := parkedReceipt(t)

	txs, err := f.harness.Execute(f.ctx, &hold, &service.PendingOverrides{
		SourceID: &f.bca.ID,
		Collapse: ptr(true),
	})
	require.NoError(t, err)
	require.Len(t, txs, 1)

	live := f.live(t)
	require.Len(t, live, 1)
	tx := live[0]
	assert.Equal(t, int64(35_000), tx.AmountMinor, "the printed total, whole")
	require.NotNil(t, tx.ExternalID)
	assert.Equal(t, "m1#0", *tx.ExternalID, "no parked line uses N=0")
	require.NotNil(t, tx.Note)
	assert.Equal(t, "Indomaret · 2 item", *tx.Note)
	require.NotNil(t, tx.CategoryID)
	assert.Equal(t, f.makanan.ID, *tx.CategoryID, "the collapsed line inherits the majority label")
	assert.Equal(t, f.bca.ID, legSourceID(t, tx.Legs))
}

// A correction naming a line the draft does not have is refused, never
// silently dropped: the sheet and the draft must be talking about the
// same receipt.
func TestDashboardConfirmRejectsUnknownLine(t *testing.T) {
	f, hold := parkedReceipt(t)

	_, err := f.harness.Execute(f.ctx, &hold, &service.PendingOverrides{
		SourceID: &f.bca.ID,
		Items:    []service.PendingItemOverride{{N: 9, CategoryID: &f.makanan.ID}},
	})

	require.Error(t, err)
	detail := detailOf(t, err)
	assert.Equal(t, "items", detail.Field)
	assert.Equal(t, "unknown_line", detail.Rule)
	assert.Empty(t, f.live(t), "a refused confirmation writes nothing")
}

func TestDashboardCollapseRejectsItemOverrides(t *testing.T) {
	f, hold := parkedReceipt(t)

	_, err := f.harness.Execute(f.ctx, &hold, &service.PendingOverrides{
		SourceID: &f.bca.ID,
		Collapse: ptr(true),
		Items:    []service.PendingItemOverride{{N: 1, CategoryID: &f.makanan.ID}},
	})

	require.Error(t, err)
	detail := detailOf(t, err)
	assert.Equal(t, "items", detail.Field)
	assert.Equal(t, "conflict", detail.Rule)
	assert.Empty(t, f.live(t))
}

// One receipt is one purchase: a corrected date moves every line.
func TestDashboardConfirmBackdatesEveryLine(t *testing.T) {
	f, hold := parkedReceipt(t)

	_, err := f.harness.Execute(f.ctx, &hold, &service.PendingOverrides{
		SourceID:     &f.bca.ID,
		OccurredDate: ptr("2026-09-10"),
	})
	require.NoError(t, err)

	want, err := service.JakartaNoon("2026-09-10")
	require.NoError(t, err)
	live := f.live(t)
	require.Len(t, live, 2)
	for _, tx := range live {
		assert.True(t, want.Equal(tx.OccurredAt), "line %q dated %s, want %s",
			*tx.Note, tx.OccurredAt, want)
	}
}
