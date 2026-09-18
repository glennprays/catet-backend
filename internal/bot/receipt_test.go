package bot

import (
	"testing"
	"time"

	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/llm"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The gate is the whole defence against a model's arithmetic: it decides
// whether a reading may become money at all, and it never lets the lines
// disagree with the printed total (ADR-0003).

var (
	testMakanan = uuid.New()
	testRumah   = uuid.New()
	testCats    = []entity.Category{
		{ID: testMakanan, Name: "Makanan & Minuman"},
		{ID: testRumah, Name: "Rumah Tangga"},
	}
	// A fixed "now" so date rules are deterministic.
	testNow = time.Date(2026, 9, 15, 10, 30, 0, 0, time.UTC)
)

func legibleRead(items ...llm.ReceiptItem) *llm.ReceiptRead {
	return &llm.ReceiptRead{
		DocumentType: "receipt", Legible: true, Merchant: "Toko Maju",
		Currency: "IDR", HasTotalLine: true, Items: items,
	}
}

func item(name string, amount int64, cat *uuid.UUID) llm.ReceiptItem {
	it := llm.ReceiptItem{Name: name, AmountMinor: amount}
	if cat != nil {
		it.CategoryID = cat
		it.CategoryConfidence = 0.8
	}
	return it
}

func buildOK(t *testing.T, r *llm.ReceiptRead) *receiptParams {
	t.Helper()
	p, reject := buildReceiptDraft(r, receiptInput{MessageID: "m1", Now: testNow, Cats: testCats})
	require.Nil(t, reject, "unexpected refusal")
	require.NotNil(t, p)
	return p
}

// sumLines is the invariant every accepted draft must satisfy.
func sumLines(p *receiptParams) int64 {
	var total int64
	for _, it := range p.Items {
		total += it.AmountMinor
	}
	return total
}

func TestReceiptItemsMatchingTotalBecomeOneLineEach(t *testing.T) {
	r := legibleRead(
		item("Beras 5kg", 65_000, &testRumah),
		item("Minyak goreng", 35_000, &testRumah),
		item("Kopi sachet", 10_000, &testMakanan),
	)
	r.TotalMinor = 110_000

	p := buildOK(t, r)

	assert.False(t, p.Inconsistent)
	require.Len(t, p.Items, 3)
	assert.Equal(t, int64(110_000), sumLines(p))
	assert.Equal(t, []int{1, 2, 3}, []int{p.Items[0].N, p.Items[1].N, p.Items[2].N})
	assert.Equal(t, "Beras 5kg", p.Items[0].Name)
	require.NotNil(t, p.Items[0].CategoryID)
	assert.Equal(t, testRumah, *p.Items[0].CategoryID)
	assert.Equal(t, "Rumah Tangga", p.Items[0].CategoryName)
	require.NotNil(t, p.Items[0].CategoryConfidence)
	assert.InDelta(t, 0.8, *p.Items[0].CategoryConfidence, 0.001)
	assert.Equal(t, "expense", p.Type)
	assert.Equal(t, "Toko Maju", p.Merchant)
}

// The SROIE 001 shape: 10.00 + 55.90 − 5.59 discount − 0.01 rounding =
// 60.30. The lines must land on the printed total exactly, with no
// negative share.
func TestReceiptDiscountAndRoundingStillSumToTotal(t *testing.T) {
	r := legibleRead(
		item("Gift box", 1_000, nil),
		item("Hamper", 5_590, nil),
	)
	r.DiscountMinor = 559
	r.RoundingMinor = -1
	r.TotalMinor = 6_030

	p := buildOK(t, r)

	assert.False(t, p.Inconsistent)
	require.Len(t, p.Items, 2)
	assert.Equal(t, int64(6_030), sumLines(p))
	for _, it := range p.Items {
		assert.Positive(t, it.AmountMinor, "line %q must stay positive", it.Name)
	}
}

func TestReceiptTaxBecomesItsOwnLine(t *testing.T) {
	r := legibleRead(
		item("Nasi goreng", 30_000, &testMakanan),
		item("Es teh", 10_000, &testMakanan),
		item("Ayam bakar", 20_000, &testMakanan),
	)
	r.TaxMinor = 6_000
	r.TotalMinor = 66_000

	p := buildOK(t, r)

	assert.False(t, p.Inconsistent)
	require.Len(t, p.Items, 4)
	assert.Equal(t, taxLineName, p.Items[3].Name)
	assert.Equal(t, int64(6_000), p.Items[3].AmountMinor)
	require.NotNil(t, p.Items[3].CategoryID)
	assert.Equal(t, testMakanan, *p.Items[3].CategoryID, "the tax line inherits the majority category")
	assert.Equal(t, int64(66_000), sumLines(p))
}

func TestReceiptInconsistentBreakdownKeepsProportionalLines(t *testing.T) {
	r := legibleRead(
		item("Beras", 10_000, &testRumah),
		item("Gula", 10_000, &testRumah),
		item("Kopi", 10_000, &testMakanan),
	)
	r.TotalMinor = 60_000 // the read lines explain half the bill

	p := buildOK(t, r)

	assert.True(t, p.Inconsistent, "the printed breakdown did not reconcile")
	require.Len(t, p.Items, 3, "the read lines survive as proportional shares")
	assert.Equal(t, []int{1, 2, 3}, []int{p.Items[0].N, p.Items[1].N, p.Items[2].N})
	assert.Equal(t,
		[]string{"Beras", "Gula", "Kopi"},
		[]string{p.Items[0].Name, p.Items[1].Name, p.Items[2].Name})
	for _, it := range p.Items {
		assert.Equal(t, int64(20_000), it.AmountMinor, "line %q is a share of the printed total", it.Name)
	}
	assert.Equal(t, int64(60_000), sumLines(p), "the lines still sum to the printed total")
	require.NotNil(t, p.Items[0].CategoryID)
	assert.Equal(t, testRumah, *p.Items[0].CategoryID)
	require.NotNil(t, p.Items[2].CategoryID)
	assert.Equal(t, testMakanan, *p.Items[2].CategoryID)
}

func TestReceiptItemWithoutPriceIsOneLineAtTheTotal(t *testing.T) {
	r := legibleRead(
		item("Beras", 10_000, &testRumah),
		item("Kopi", 0, &testMakanan),
	)
	r.TotalMinor = 45_000

	p := buildOK(t, r)

	require.Len(t, p.Items, 1, "an unpriced item is no weight to split by")
	assert.True(t, p.Inconsistent)
	assert.Equal(t, int64(45_000), p.Items[0].AmountMinor)
	assert.Equal(t, "Toko Maju · 2 item", p.Items[0].Name)
}

func TestReceiptWithoutItemsIsOneLineAtTheTotal(t *testing.T) {
	r := legibleRead()
	r.TotalMinor = 45_000

	p := buildOK(t, r)

	require.Len(t, p.Items, 1)
	assert.False(t, p.Inconsistent, "no printed breakdown is not an inconsistent one")
	assert.Equal(t, int64(45_000), p.Items[0].AmountMinor)
}

func TestReceiptWithoutTotalLineIsRefused(t *testing.T) {
	r := legibleRead(item("Beras", 10_000, nil))
	r.HasTotalLine = false

	p, reject := buildReceiptDraft(r, receiptInput{MessageID: "m1", Now: testNow, Cats: testCats})

	assert.Nil(t, p)
	require.NotNil(t, reject)
	assert.Equal(t, "photo_no_total", reject.Template)
}

func TestIllegiblePhotoIsRefusedWithItsReason(t *testing.T) {
	r := legibleRead()
	r.Legible = false
	r.Issues = []string{"blur", "too_small"}
	r.TotalMinor = 45_000

	p, reject := buildReceiptDraft(r, receiptInput{MessageID: "m1", Now: testNow, Cats: testCats})

	assert.Nil(t, p)
	require.NotNil(t, reject)
	assert.Equal(t, "photo_unreadable", reject.Template)
	assert.Equal(t, "blur", reject.Data["Issue"])
}

func TestForeignCurrencyIsRefused(t *testing.T) {
	r := legibleRead(item("Gift box", 1_000, nil))
	r.Currency = "MYR"
	r.TotalMinor = 1_000

	p, reject := buildReceiptDraft(r, receiptInput{MessageID: "m1", Now: testNow, Cats: testCats})

	assert.Nil(t, p)
	require.NotNil(t, reject)
	assert.Equal(t, "photo_currency", reject.Template)
	assert.Equal(t, "MYR", reject.Data["Currency"])
}

func TestStatementAndNonReceiptAreRefused(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mutate   func(*llm.ReceiptRead)
		template string
	}{
		{"statement", func(r *llm.ReceiptRead) { r.DocumentType = "statement" }, "photo_statement"},
		{"other", func(r *llm.ReceiptRead) { r.DocumentType = "other" }, "photo_not_receipt"},
		{"flagged", func(r *llm.ReceiptRead) { r.Issues = []string{"not_a_receipt"} }, "photo_not_receipt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := legibleRead(item("Beras", 10_000, nil))
			r.TotalMinor = 10_000
			tc.mutate(r)

			p, reject := buildReceiptDraft(r, receiptInput{MessageID: "m1", Now: testNow, Cats: testCats})

			assert.Nil(t, p)
			require.NotNil(t, reject)
			assert.Equal(t, tc.template, reject.Template)
		})
	}
}

// A model that reports it never got the image is an outage of ours, not
// a verdict on the user's photo.
func TestVisionModelThatSawNoImageIsAnOutageNotARefusal(t *testing.T) {
	p, reject := buildReceiptDraft(&llm.ReceiptRead{DocumentType: "other", Legible: false},
		receiptInput{MessageID: "m1", Now: testNow, Cats: testCats})

	assert.Nil(t, p)
	require.NotNil(t, reject)
	assert.Equal(t, "photo_vision_down", reject.Template)
}

// The model saw a cat: that is still a refusal, in the old words.
func TestNonReceiptPhotoIsStillRefusedAsNotAReceipt(t *testing.T) {
	p, reject := buildReceiptDraft(&llm.ReceiptRead{ImageReceived: true, DocumentType: "other", Legible: false},
		receiptInput{MessageID: "m1", Now: testNow, Cats: testCats})

	assert.Nil(t, p)
	require.NotNil(t, reject)
	assert.Equal(t, "photo_not_receipt", reject.Template)
}

// A model that forgot the flag but read the paper is trusted: the guard
// is conjunctive, so money on the page wins.
func TestReadWithoutTheFlagButWithMoneyIsBelieved(t *testing.T) {
	r := legibleRead(item("Beras", 10_000, nil))
	r.TotalMinor = 10_000

	p, reject := buildReceiptDraft(r, receiptInput{MessageID: "m1", Now: testNow, Cats: testCats})

	assert.Nil(t, reject)
	require.NotNil(t, p)
	assert.False(t, r.ImageReceived)
	require.Len(t, p.Items, 1)
}

func TestReceiptDateOutsideTheWindowIsDropped(t *testing.T) {
	for _, tc := range []struct {
		name string
		date string
	}{
		// A year and a bit: printed dates up to MaxBackdateDays are now
		// believed, so only something older is a hallucination.
		{"too old", testNow.AddDate(0, 0, -400).Format("2006-01-02")},
		{"in the future", testNow.AddDate(0, 0, 3).Format("2006-01-02")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := legibleRead(item("Beras", 45_000, nil))
			r.TotalMinor = 45_000
			r.Date = tc.date

			p := buildOK(t, r)

			assert.Empty(t, p.ReceiptDate, "an out-of-window printed date is not stored")
			assert.Equal(t, testNow.UTC(), p.OccurredAt)
		})
	}
}

func TestReceiptDateInsideTheWindowAnchorsAtJakartaNoon(t *testing.T) {
	r := legibleRead(item("Beras", 45_000, nil))
	r.TotalMinor = 45_000
	r.Date = "2026-09-13"

	p := buildOK(t, r)

	assert.Equal(t, "2026-09-13", p.ReceiptDate)
	assert.Equal(t, "2026-09-13T12:00:00+07:00", p.OccurredAt.Format(time.RFC3339))
}

func TestPaymentScreenshotIsOneLineNamedAfterTheCounterparty(t *testing.T) {
	r := &llm.ReceiptRead{
		DocumentType: "payment_screenshot", Legible: true, Currency: "IDR",
		HasTotalLine: true, TotalMinor: 35_000, Merchant: "GoPay",
		Counterparty: "Warung Bu Sri", Direction: "out",
		Items: []llm.ReceiptItem{item("Warung Bu Sri", 35_000, &testMakanan)},
	}

	p := buildOK(t, r)

	require.Len(t, p.Items, 1)
	assert.Equal(t, "Warung Bu Sri", p.Items[0].Name)
	assert.Equal(t, "Warung Bu Sri", p.Merchant)
	assert.Equal(t, int64(35_000), p.Items[0].AmountMinor)
	assert.False(t, p.Inconsistent)
	assert.Equal(t, "payment_screenshot", p.DocumentType)
}

func TestMerchantlessReceiptStillHasALabel(t *testing.T) {
	r := legibleRead(item("", 45_000, nil))
	r.Merchant = ""
	r.TotalMinor = 45_000

	p := buildOK(t, r)

	assert.Equal(t, "Struk", p.Merchant)
	assert.Equal(t, "Struk", p.Items[0].Name, "an unnamed line falls back to the merchant label")
}

// allocate is the money-exactness primitive: whatever the weights, the
// shares must add up to the target and never go negative.
func TestAllocateAlwaysSumsToTheTarget(t *testing.T) {
	cases := []struct {
		total   int64
		weights []int64
	}{
		{100, []int64{1, 1, 1}},
		{6_030, []int64{1_000, 5_590}},
		{1, []int64{5, 5, 5}},
		{999_999, []int64{7, 11, 13, 17}},
		{45_000, []int64{45_000}},
	}
	for _, c := range cases {
		shares := allocate(c.total, c.weights)
		require.Len(t, shares, len(c.weights))
		var sum int64
		for _, s := range shares {
			assert.GreaterOrEqual(t, s, int64(0))
			sum += s
		}
		assert.Equal(t, c.total, sum, "shares of %d over %v", c.total, c.weights)
	}
}

// A split bill is the one case where the printed lines have to be cut:
// the shares are money facts, the lines are only how the money is
// described, so the lines bend and the total never does.
func TestSplitLinesCutsOnlyTheStraddlingLine(t *testing.T) {
	items := []receiptItem{
		{N: 1, Name: "a", AmountMinor: 10_000},
		{N: 2, Name: "b", AmountMinor: 9_000},
		{N: 3, Name: "c", AmountMinor: 8_000},
		{N: 4, Name: "d", AmountMinor: 5_000},
		{N: 5, Name: "e", AmountMinor: 4_000},
		{N: 6, Name: "f", AmountMinor: 2_000},
	}
	cash, bca := uuid.New(), uuid.New()
	payments := []receiptPayment{
		{SourceID: cash, SourceName: "Cash", AmountMinor: 20_000},
		{SourceID: bca, SourceName: "BCA", AmountMinor: 18_000},
	}

	out := splitLines(items, payments)

	require.Len(t, out, 7, "exactly one line is cut in two")
	var total int64
	perSource := map[uuid.UUID]int64{}
	for _, line := range out {
		require.NotNil(t, line.SourceID, "every line names the Source that paid it")
		total += line.AmountMinor
		perSource[*line.SourceID] += line.AmountMinor
	}
	assert.Equal(t, int64(38_000), total)
	assert.Equal(t, int64(20_000), perSource[cash])
	assert.Equal(t, int64(18_000), perSource[bca])

	// Line 3 straddles the boundary: 1.000 on Cash, 7.000 on BCA.
	var halves []receiptItem
	for _, line := range out {
		if line.N == 3 {
			halves = append(halves, line)
		}
	}
	require.Len(t, halves, 2)
	assert.Equal(t, "a", halves[0].Part)
	assert.Equal(t, int64(1_000), halves[0].AmountMinor)
	assert.Equal(t, cash, *halves[0].SourceID)
	assert.Equal(t, "b", halves[1].Part)
	assert.Equal(t, int64(7_000), halves[1].AmountMinor)
	assert.Equal(t, bca, *halves[1].SourceID)

	// The receipt's own order survives.
	for i := 1; i < len(out); i++ {
		assert.LessOrEqual(t, out[i-1].N, out[i].N)
	}
}

func splitFixture() (*receiptParams, []entity.Source) {
	cash := entity.Source{ID: uuid.New(), Name: "Cash", Type: "cash"}
	bca := entity.Source{ID: uuid.New(), Name: "BCA", Type: "bank"}
	p := &receiptParams{
		Type: "expense", Merchant: "Indomaret", TotalMinor: 38_000,
		Items: []receiptItem{
			{N: 1, Name: "a", AmountMinor: 20_000},
			{N: 2, Name: "b", AmountMinor: 18_000},
		},
	}
	return p, []entity.Source{cash, bca}
}

func TestApplySplitRefusesSharesThatMissTheTotal(t *testing.T) {
	p, sources := splitFixture()

	reject := applySplit(p, []llm.Payment{
		{SourceID: &sources[0].ID, AmountText: "20rb"},
		{SourceID: &sources[1].ID, AmountText: "15rb"},
	}, sources)

	require.NotNil(t, reject)
	assert.Equal(t, "receipt_split_mismatch", reject.Template)
	assert.Equal(t, "Rp20.000 + Rp15.000", reject.Data["Parts"])
	assert.Equal(t, "Rp38.000", reject.Data["Total"])
	assert.Empty(t, p.Payments, "a refused split changes nothing")
}

func TestApplySplitFillsTheRemainder(t *testing.T) {
	p, sources := splitFixture()

	reject := applySplit(p, []llm.Payment{
		{SourceID: &sources[0].ID, AmountText: "20rb"},
		{SourceID: &sources[1].ID, AmountText: ""},
	}, sources)

	require.Nil(t, reject)
	require.Len(t, p.Payments, 2)
	assert.Equal(t, int64(20_000), p.Payments[0].AmountMinor)
	assert.Equal(t, int64(18_000), p.Payments[1].AmountMinor, "sisanya is the rest of the printed total")
	assert.Equal(t, basisUser, p.SourceBasis)
	require.NotNil(t, p.SourceID)
	assert.Equal(t, sources[0].ID, *p.SourceID)
	assert.Equal(t, int64(38_000), sumLines(p))
}

func TestApplySplitRefusesAnUnknownSource(t *testing.T) {
	p, sources := splitFixture()

	reject := applySplit(p, []llm.Payment{
		{SourceID: nil, AmountText: "20rb"},
		{SourceID: &sources[1].ID, AmountText: ""},
	}, sources)

	require.NotNil(t, reject)
	assert.Equal(t, "receipt_split_unknown_source", reject.Template)
}
