package llm_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glennprays/catet-backend/config"
	"github.com/glennprays/catet-backend/internal/llm"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// contentServer replies with a fixed assistant content payload and
// records every request body it saw.
func contentServer(t *testing.T, content string, calls *int32, bodies *[]map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		if bodies != nil {
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			*bodies = append(*bodies, body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":` + jsonString(content) + `}}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func newTestClient(url string) *llm.Client {
	return llm.New(url, "test-key", "test-model", 2*time.Second)
}

func TestPickCategoryValid(t *testing.T) {
	food := llm.Candidate{ID: uuid.New(), Name: "Makanan & Minuman", Hint: "Kebutuhan"}
	transport := llm.Candidate{ID: uuid.New(), Name: "Transportasi"}

	var calls int32
	var bodies []map[string]any
	srv := contentServer(t, `{"category_id":"`+food.ID.String()+`","confidence":0.82}`, &calls, &bodies)

	id, conf, err := newTestClient(srv.URL).PickCategory(context.Background(), "kopi kenangan", "", "expense", []llm.Candidate{food, transport})
	require.NoError(t, err)
	require.NotNil(t, id)
	assert.Equal(t, food.ID, *id)
	assert.InDelta(t, 0.82, conf, 0.0001)
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls))

	require.Len(t, bodies, 1)
	assert.Equal(t, "test-model", bodies[0]["model"])
	format, ok := bodies[0]["response_format"].(map[string]any)
	require.True(t, ok)
	// json_object, not strict json_schema: routers accept the schema and
	// then answer in prose. The schema travels in the system prompt and
	// the whitelist check enforces the answer.
	assert.Equal(t, "json_object", format["type"])
	assert.NotContains(t, format, "json_schema")
	messages, ok := bodies[0]["messages"].([]any)
	require.True(t, ok)
	system, ok := messages[0].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, system["content"], "category_id")
	assert.Contains(t, system["content"], "JSON")
}

// Real routers wrap the object in a fence or pad it with a sentence, and
// some append an SSE terminator to a non-stream response. Both must
// still resolve to a pick.
func TestPickCategoryToleratesDirtyResponses(t *testing.T) {
	food := llm.Candidate{ID: uuid.New(), Name: "Makanan & Minuman"}
	content := "```json\n{\"category_id\":\"" + food.ID.String() + "\",\"confidence\":0.9}\n```"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":` + jsonString(content) + `}}]}` + "data: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)

	id, conf, err := newTestClient(srv.URL).PickCategory(context.Background(), "kopi", "", "expense", []llm.Candidate{food})
	require.NoError(t, err)
	require.NotNil(t, id)
	assert.Equal(t, food.ID, *id)
	assert.InDelta(t, 0.9, conf, 0.0001)
}

func TestPickCategoryUnknownID(t *testing.T) {
	food := llm.Candidate{ID: uuid.New(), Name: "Makanan & Minuman"}

	cases := []struct {
		name    string
		content string
	}{
		{"id outside the candidate list", `{"category_id":"` + uuid.New().String() + `","confidence":0.99}`},
		{"not a uuid at all", `{"category_id":"makanan","confidence":0.99}`},
		{"explicit null", `{"category_id":null,"confidence":0}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var calls int32
			srv := contentServer(t, c.content, &calls, nil)

			id, conf, err := newTestClient(srv.URL).PickCategory(context.Background(), "kopi", "", "expense", []llm.Candidate{food})
			require.NoError(t, err)
			assert.Nil(t, id)
			assert.Zero(t, conf)
		})
	}
}

func TestPickSource(t *testing.T) {
	spay := llm.Candidate{ID: uuid.New(), Name: "SPayLater", Hint: "credit"}
	bca := llm.Candidate{ID: uuid.New(), Name: "BCA Tahapan", Hint: "bank"}

	var calls int32
	srv := contentServer(t, `{"source_id":"`+spay.ID.String()+`"}`, &calls, nil)

	id, err := newTestClient(srv.URL).PickSource(context.Background(), "spay", []llm.Candidate{spay, bca})
	require.NoError(t, err)
	require.NotNil(t, id)
	assert.Equal(t, spay.ID, *id)
}

func TestRetryBudgetOnServerError(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()

	cat := llm.Candidate{ID: uuid.New(), Name: "Makanan"}
	id, conf, err := newTestClient(srv.URL).PickCategory(context.Background(), "kopi", "", "expense", []llm.Candidate{cat})
	require.Error(t, err)
	assert.Nil(t, id)
	assert.Zero(t, conf)
	assert.Equal(t, int32(2), atomic.LoadInt32(&calls), "one retry only")
}

func TestNoRetryOnClientError(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad key"}`))
	}))
	defer srv.Close()

	src := llm.Candidate{ID: uuid.New(), Name: "Cash"}
	id, err := newTestClient(srv.URL).PickSource(context.Background(), "cash", []llm.Candidate{src})
	require.Error(t, err)
	assert.Nil(t, id)
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls), "4xx is fatal")
}

func TestVerdictGuard(t *testing.T) {
	long := "Pengeluaranmu minggu ini terasa lebih ramai dari biasanya dan pola belanja harianmu ikut naik cukup terasa juga"

	cases := []struct {
		name    string
		verdict string
		want    string
	}{
		{"clean sentence passes", "Belanja harianmu masih terkendali.", "Belanja harianmu masih terkendali."},
		{"digits rejected", "Kamu habis 250 hari ini.", ""},
		{"rupiah rejected", "Sisa Rp banyak.", ""},
		{"shorthand rejected", "Naik rb dari minggu lalu.", ""},
		{"too long rejected", long, ""},
		{"empty stays empty", "   ", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var calls int32
			srv := contentServer(t, `{"verdict":`+jsonString(c.verdict)+`}`, &calls, nil)

			got, err := newTestClient(srv.URL).Verdict(context.Background(), "id", map[string]string{"spent": "Rp250.000"})
			require.NoError(t, err)
			assert.Equal(t, c.want, got)
		})
	}
}

// TestTimeoutRetriesThenFails stalls the endpoint past the per-attempt
// deadline: the client must give up after exactly one retry.
func TestTimeoutRetriesThenFails(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		select {
		case <-time.After(500 * time.Millisecond):
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	client := llm.New(srv.URL, "k", "m", 50*time.Millisecond)
	_, err := client.Verdict(context.Background(), "id", nil)
	require.Error(t, err)
	assert.Equal(t, int32(2), atomic.LoadInt32(&calls))
}

func TestProvideAndNilReceiver(t *testing.T) {
	assert.Nil(t, llm.Provide(&config.Config{}))

	client := llm.Provide(&config.Config{LlmAPIKey: "sk-test", LlmBaseURL: "https://example.test/v1", LlmModel: "gpt", LlmTimeoutSecs: 5})
	require.NotNil(t, client)

	var absent *llm.Client
	id, conf, err := absent.PickCategory(context.Background(), "kopi", "", "expense", nil)
	assert.Nil(t, id)
	assert.Zero(t, conf)
	assert.ErrorIs(t, err, llm.ErrUnavailable)

	sid, err := absent.PickSource(context.Background(), "bca", nil)
	assert.Nil(t, sid)
	assert.ErrorIs(t, err, llm.ErrUnavailable)

	verdict, err := absent.Verdict(context.Background(), "id", nil)
	assert.Empty(t, verdict)
	assert.ErrorIs(t, err, llm.ErrUnavailable)
}

// A receipt request must arrive as a multimodal message: the prompt as
// text, the photo as an inline data URL, addressed to the vision model.
func TestReadReceiptSendsAnInlineImageToTheVisionModel(t *testing.T) {
	food := llm.Candidate{ID: uuid.New(), Name: "Makanan & Minuman"}

	var calls int32
	var bodies []map[string]any
	srv := contentServer(t, `{
		"document_type":"receipt","legible":true,"issues":[],
		"merchant":" Indomaret ","date":"2026-09-13","currency":"idr",
		"has_total_line":true,"total_minor":35000,"subtotal_minor":35000,
		"discount_minor":0,"tax_minor":0,"rounding_minor":0,
		"payment_method":"DEBIT BCA","direction":"","counterparty":"",
		"items":[
			{"name":"Indomie","qty":2,"amount_minor":20000,"category_id":"`+food.ID.String()+`","category_confidence":1.7},
			{"name":"Aqua","qty":1,"amount_minor":-15000,"category_id":"11111111-1111-1111-1111-111111111111","category_confidence":0.9}
		]
	}`, &calls, &bodies)

	client := llm.New(srv.URL, "test-key", "test-model", 2*time.Second).
		WithVision("vision-model", 3*time.Second)
	read, err := client.ReadReceipt(context.Background(), []byte{0xff, 0xd8, 0xff}, "image/jpeg", "pakai bca", []llm.Candidate{food})
	require.NoError(t, err)

	require.Len(t, bodies, 1)
	assert.Equal(t, "vision-model", bodies[0]["model"])
	messages, ok := bodies[0]["messages"].([]any)
	require.True(t, ok)
	require.Len(t, messages, 2)
	parts, ok := messages[1].(map[string]any)["content"].([]any)
	require.True(t, ok, "the user message must be multimodal")
	require.Len(t, parts, 2)
	assert.Equal(t, "text", parts[0].(map[string]any)["type"])
	assert.Contains(t, parts[0].(map[string]any)["text"], "pakai bca")
	image := parts[1].(map[string]any)
	assert.Equal(t, "image_url", image["type"])
	url, _ := image["image_url"].(map[string]any)["url"].(string)
	assert.True(t, strings.HasPrefix(url, "data:image/jpeg;base64,"), "url was %q", url)

	// Post-processing is the whitelist and the sanity clamp: an id we
	// never offered is dropped, a confidence above 1 is clamped, a
	// negative amount becomes zero.
	assert.Equal(t, "Indomaret", read.Merchant)
	assert.Equal(t, "IDR", read.Currency)
	assert.Equal(t, "2026-09-13", read.Date)
	require.Len(t, read.Items, 2)
	require.NotNil(t, read.Items[0].CategoryID)
	assert.Equal(t, food.ID, *read.Items[0].CategoryID)
	assert.InDelta(t, 1.0, read.Items[0].CategoryConfidence, 0.0001)
	assert.Nil(t, read.Items[1].CategoryID, "an id outside the whitelist is no answer")
	assert.Zero(t, read.Items[1].AmountMinor)
}

// An unparseable date is dropped rather than repaired, and usage is
// reported so the cost of a photo is visible in the logs.
func TestReadReceiptDropsBadDatesAndReportsUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"document_type\":\"receipt\",\"legible\":true,\"date\":\"13/09/2026\",\"has_total_line\":true,\"total_minor\":1000,\"items\":[]}"}}],
		                        "usage":{"prompt_tokens":1234,"completion_tokens":567}}`))
	}))
	defer srv.Close()

	read, err := newTestClient(srv.URL).ReadReceipt(context.Background(), []byte{1}, "image/png", "", nil)
	require.NoError(t, err)
	assert.Empty(t, read.Date)
	assert.Equal(t, 1234, read.PromptTokens)
	assert.Equal(t, 567, read.CompletionTokens)
}

// WithVision is a fallback, not a requirement: an empty model or a
// non-positive timeout keeps the text defaults.
func TestWithVisionFallsBackToTheTextModel(t *testing.T) {
	var calls int32
	var bodies []map[string]any
	srv := contentServer(t, `{"document_type":"other","legible":false}`, &calls, &bodies)

	client := llm.New(srv.URL, "test-key", "text-model", 2*time.Second).WithVision("", 0)
	_, err := client.ReadReceipt(context.Background(), []byte{1}, "image/jpeg", "", nil)
	require.NoError(t, err)
	require.Len(t, bodies, 1)
	assert.Equal(t, "text-model", bodies[0]["model"])

	var absent *llm.Client
	assert.Nil(t, absent.WithVision("m", time.Second))
	read, err := absent.ReadReceipt(context.Background(), nil, "", "", nil)
	assert.Nil(t, read)
	assert.ErrorIs(t, err, llm.ErrUnavailable)
}
