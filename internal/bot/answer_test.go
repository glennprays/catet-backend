package bot

import (
	"testing"

	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The fast path is the whole point of the round-2 work: a message the
// rules can explain word for word must never cost a model call, and
// "bayar pake BCA, ya catat" is such a message — it confirms the draft
// and names the Source in one breath.
func TestParseBareAnswer(t *testing.T) {
	bca := entity.Source{ID: uuid.New(), Name: "BCA", Type: "bank"}
	gopay := entity.Source{ID: uuid.New(), Name: "GoPay", Type: "ewallet"}
	sources := []entity.Source{bca, gopay}

	cases := []struct {
		name    string
		text    string
		expect  string
		confirm string
		source  *uuid.UUID
		amount  int64
		nilOut  bool
	}{
		{name: "confirm with a source clause", text: "bayar pake BCA, ya catat",
			expect: "yes_no", confirm: "yes", source: &bca.ID},
		{name: "bare yes", text: "ya", expect: "yes_no", confirm: "yes"},
		{name: "bare no", text: "tidak", expect: "yes_no", confirm: "no"},
		{name: "bare source name", text: "bca", expect: "source", source: &bca.ID},
		{name: "confirm with a from clause", text: "oke dari gopay",
			expect: "yes_no", confirm: "yes", source: &gopay.ID},
		{name: "a category is not a rules answer", text: "kategorinya makanan",
			expect: "yes_no", nilOut: true},
		{name: "an unknown alias belongs to the router", text: "pakai jago",
			expect: "source", nilOut: true},
		{name: "amount when one was asked for", text: "25rb", expect: "amount", amount: 25_000},
		{name: "amount nobody asked for", text: "25rb", expect: "yes_no", nilOut: true},
		{name: "empty", text: "   ", expect: "yes_no", nilOut: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseBareAnswer(c.text, c.expect, sources)
			if c.nilOut {
				assert.Nil(t, got, "the rules must defer to the router")
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, c.confirm, got.Confirm)
			if c.source == nil {
				assert.Nil(t, got.SourceID)
			} else {
				require.NotNil(t, got.SourceID)
				assert.Equal(t, *c.source, *got.SourceID)
			}
			if c.amount == 0 {
				assert.Nil(t, got.AmountMinor)
			} else {
				require.NotNil(t, got.AmountMinor)
				assert.Equal(t, c.amount, *got.AmountMinor)
			}
		})
	}
}

// A message that says both yes and no says nothing the rules can act on.
func TestParseBareAnswerRefusesContradictions(t *testing.T) {
	sources := []entity.Source{{ID: uuid.New(), Name: "BCA", Type: "bank"}}
	assert.Nil(t, parseBareAnswer("ya tidak", "yes_no", sources))
}
