package llm_test

import (
	"context"
	"errors"
	"testing"

	"github.com/glennprays/catet-backend/internal/llm"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func intentFixture() (llm.IntentRequest, llm.Candidate, llm.Candidate) {
	bca := llm.Candidate{ID: uuid.New(), Name: "BCA", Hint: "bank"}
	food := llm.Candidate{ID: uuid.New(), Name: "Makanan & Minuman", Hint: "Kebutuhan"}
	return llm.IntentRequest{
		Lang:  "id",
		Text:  "itu kategori makanan",
		Today: "2026-09-15",
		Recent: []llm.RecentItem{
			{N: 1, Type: "expense", Merchant: "ayam crispi", Amount: "Rp25.000", Category: "Lainnya", Source: "BCA"},
		},
		Sources:    []llm.Candidate{bca},
		Categories: []llm.Candidate{food},
	}, bca, food
}

func TestIntentCorrection(t *testing.T) {
	req, _, food := intentFixture()
	var bodies []map[string]any
	srv := contentServer(t, `{"intent":"correct","drafts":[],"correction":{"target_index":1,"category_id":"`+
		food.ID.String()+`","source_id":null,"merchant":null,"amount_text":null,"day_offset":null},`+
		`"undo_index":0,"period":"","balance_source_id":null,"answer":null}`, new(int32), &bodies)

	got, err := newTestClient(srv.URL).Intent(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, llm.KindCorrect, got.Kind)
	require.NotNil(t, got.Correction)
	assert.Equal(t, 1, got.Correction.TargetIndex)
	require.NotNil(t, got.Correction.CategoryID)
	assert.Equal(t, food.ID, *got.Correction.CategoryID)
	assert.Nil(t, got.Correction.SourceID)
	assert.Empty(t, got.Drafts)

	// The prompt must carry the open-question-free context the router
	// reasons over: the recent batch and both whitelists.
	require.Len(t, bodies, 1)
	messages, ok := bodies[0]["messages"].([]any)
	require.True(t, ok)
	user, ok := messages[1].(map[string]any)
	require.True(t, ok)
	content, ok := user["content"].(string)
	require.True(t, ok)
	assert.Contains(t, content, "RECENT (most recent last):")
	assert.Contains(t, content, "1. expense · Rp25.000 · ayam crispi")
	assert.Contains(t, content, food.ID.String())
	assert.Contains(t, content, "MESSAGE: itu kategori makanan")
	assert.NotContains(t, content, "OPEN QUESTION")
}

// An id outside the whitelist is dropped without invalidating the rest
// of the answer: the intent still stands, the source simply is unknown
// (ADR-0003).
func TestIntentDropsUnlistedID(t *testing.T) {
	req, _, food := intentFixture()
	req.Text = "kopi 25k pakai gopay"
	srv := contentServer(t, `{"intent":"capture","drafts":[{"type":"expense","amount_text":"25k","merchant":"kopi",`+
		`"source_id":"`+uuid.New().String()+`","destination_source_id":null,"category_id":"`+food.ID.String()+
		`","day_offset":3}],"correction":null,"undo_index":0,"period":"","balance_source_id":null,"answer":null}`,
		new(int32), nil)

	got, err := newTestClient(srv.URL).Intent(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, llm.KindCapture, got.Kind)
	require.Len(t, got.Drafts, 1)
	assert.Nil(t, got.Drafts[0].SourceID, "an id we never offered is no answer")
	require.NotNil(t, got.Drafts[0].CategoryID)
	assert.Equal(t, food.ID, *got.Drafts[0].CategoryID)
	assert.Equal(t, "25k", got.Drafts[0].AmountText)
	assert.Equal(t, 0, got.Drafts[0].DayOffset, "a future date is never a capture date")
}

// One chat message may settle several things about the open draft at
// once: "20rb cash sisanya bca, ya" confirms and splits the payment.
func TestIntentAnswerCarriesEveryField(t *testing.T) {
	req, bca, food := intentFixture()
	cash := llm.Candidate{ID: uuid.New(), Name: "Cash", Hint: "cash"}
	req.Sources = append(req.Sources, cash)
	req.Text = "20rb cash sisanya bca, ya"
	req.Open = &llm.OpenQuestion{Expect: "yes_no", Summary: "Struk Indomaret · Rp38.000"}
	srv := contentServer(t, `{"intent":"answer","drafts":[],"correction":null,"undo_index":0,`+
		`"period":"","balance_source_id":null,"answer":{"confirm":"yes","source_id":null,`+
		`"payments":[{"source_id":"`+cash.ID.String()+`","amount_text":"20rb"},`+
		`{"source_id":"`+bca.ID.String()+`","amount_text":""}],`+
		`"category_id":"`+food.ID.String()+`","merchant":"Indomaret","day_offset":-1}}`,
		new(int32), nil)

	got, err := newTestClient(srv.URL).Intent(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, llm.KindAnswer, got.Kind)
	require.NotNil(t, got.Answer)
	assert.Equal(t, "yes", got.Answer.Confirm)
	require.Len(t, got.Answer.Payments, 2)
	require.NotNil(t, got.Answer.Payments[0].SourceID)
	assert.Equal(t, cash.ID, *got.Answer.Payments[0].SourceID)
	assert.Equal(t, "20rb", got.Answer.Payments[0].AmountText)
	require.NotNil(t, got.Answer.Payments[1].SourceID)
	assert.Equal(t, bca.ID, *got.Answer.Payments[1].SourceID)
	assert.Empty(t, got.Answer.Payments[1].AmountText, "an empty amount is the remainder")
	require.NotNil(t, got.Answer.CategoryID)
	assert.Equal(t, food.ID, *got.Answer.CategoryID)
	require.NotNil(t, got.Answer.Merchant)
	assert.Equal(t, "Indomaret", *got.Answer.Merchant)
	require.NotNil(t, got.Answer.DayOffset)
	assert.Equal(t, -1, *got.Answer.DayOffset)
}

// An answer object that settles nothing is not an answer at all.
func TestIntentEmptyAnswerIsNil(t *testing.T) {
	req, _, _ := intentFixture()
	req.Open = &llm.OpenQuestion{Expect: "yes_no", Summary: "Struk"}
	srv := contentServer(t, `{"intent":"answer","drafts":[],"correction":null,"undo_index":0,`+
		`"period":"","balance_source_id":null,"answer":{"confirm":"","source_id":null,"payments":[],`+
		`"category_id":null,"merchant":null,"day_offset":null}}`, new(int32), nil)

	got, err := newTestClient(srv.URL).Intent(context.Background(), req)
	require.NoError(t, err)
	assert.Nil(t, got.Answer)
}

func TestIntentUnknownKind(t *testing.T) {
	req, _, _ := intentFixture()
	srv := contentServer(t, `{"intent":"lol","drafts":[],"correction":null,"undo_index":0,`+
		`"period":"","balance_source_id":null,"answer":null}`, new(int32), nil)

	got, err := newTestClient(srv.URL).Intent(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, llm.KindUnknown, got.Kind)
}

func TestIntentNilClient(t *testing.T) {
	var c *llm.Client
	_, err := c.Intent(context.Background(), llm.IntentRequest{})
	assert.True(t, errors.Is(err, llm.ErrUnavailable))
}
