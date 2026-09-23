package parse_test

import (
	"testing"
	"time"

	"github.com/glennprays/catet-backend/internal/bot/parse"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// jakarta mirrors Asia/Jakarta without depending on the host tzdb; the
// parser only has to preserve whatever location the caller passes.
var jakarta = time.FixedZone("Asia/Jakarta", 7*60*60)

// now is a Tuesday afternoon, deliberately mid-month so "tanggal 5" is
// in the past and "tanggal 28" is not.
var now = time.Date(2026, time.September, 15, 15, 30, 0, 0, jakarta)

type wantDraft struct {
	typ      string
	amount   int64
	merchant string
	pays     []string
	froms    []string
	places   []string
	dest     string
	hold     string
	lang     string
	at       time.Time // zero means "unchanged from now"
}

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		cmd     string
		batalN  int
		lang    string
		drafts  []wantDraft
		noDraft bool
	}{
		{
			name:   "simple expense",
			input:  "kopi 25rb",
			lang:   "id",
			drafts: []wantDraft{{typ: "expense", amount: 25_000, merchant: "kopi", lang: "id"}},
		},
		{
			name:   "expense with explicit source",
			input:  "makan siang 30k pakai gopay",
			lang:   "id",
			drafts: []wantDraft{{typ: "expense", amount: 30_000, merchant: "makan siang", pays: []string{"gopay"}, lang: "id"}},
		},
		{
			name:  "two expenses in one message",
			input: "kopi 25k, gojek 18k",
			lang:  "id",
			drafts: []wantDraft{
				{typ: "expense", amount: 25_000, merchant: "kopi", lang: "id"},
				{typ: "expense", amount: 18_000, merchant: "gojek", lang: "id"},
			},
		},
		{
			name:  "physical lines with a bare source footer are two expenses",
			input: "sarapan bubur ayam 16k\ncilok 10k\nBRI",
			lang:  "id",
			drafts: []wantDraft{
				{typ: "expense", amount: 16_000, merchant: "sarapan bubur ayam", lang: "id"},
				{typ: "expense", amount: 10_000, merchant: "cilok", lang: "id"},
			},
		},
		{
			name:  "physical lines with a dari source footer are two expenses",
			input: "nasi daun jeruk 22k\nes podeng 12k\nDari bri",
			lang:  "id",
			drafts: []wantDraft{
				{typ: "expense", amount: 22_000, merchant: "nasi daun jeruk", lang: "id"},
				{typ: "expense", amount: 12_000, merchant: "es podeng", lang: "id"},
			},
		},
		{
			name:  "expense plus transfer",
			input: "bensin 20k cash, pindah 500k ke gopay",
			lang:  "id",
			drafts: []wantDraft{
				// The bare trailing "cash" stays in the merchant: the
				// harness, not the parser, matches it to a Source.
				{typ: "expense", amount: 20_000, merchant: "bensin cash", lang: "id"},
				{typ: "transfer", amount: 500_000, merchant: "", dest: "gopay", lang: "id"},
			},
		},
		{
			name:   "income",
			input:  "gajian 8jt",
			lang:   "id",
			drafts: []wantDraft{{typ: "income", amount: 8_000_000, merchant: "gajian", lang: "id"}},
		},
		{
			name:  "backdated yesterday",
			input: "kemarin makan 50k",
			lang:  "id",
			drafts: []wantDraft{{
				typ: "expense", amount: 50_000, merchant: "makan", lang: "id",
				at: time.Date(2026, time.September, 14, 12, 0, 0, 0, jakarta),
			}},
		},
		{
			name:  "this morning",
			input: "tadi pagi parkir 5k",
			lang:  "id",
			drafts: []wantDraft{{
				typ: "expense", amount: 5_000, merchant: "parkir", lang: "id",
				at: time.Date(2026, time.September, 15, 8, 0, 0, 0, jakarta),
			}},
		},
		{
			name:  "last night",
			input: "semalam nonton 50rb",
			lang:  "id",
			drafts: []wantDraft{{
				typ: "expense", amount: 50_000, merchant: "nonton", lang: "id",
				at: time.Date(2026, time.September, 14, 20, 0, 0, 0, jakarta),
			}},
		},
		{
			name:  "two days ago",
			input: "2 hari lalu bensin 100k",
			lang:  "id",
			drafts: []wantDraft{{
				typ: "expense", amount: 100_000, merchant: "bensin", lang: "id",
				at: time.Date(2026, time.September, 13, 12, 0, 0, 0, jakarta),
			}},
		},
		{
			name:  "explicit day this month",
			input: "tanggal 5 listrik 300rb",
			lang:  "id",
			drafts: []wantDraft{{
				typ: "expense", amount: 300_000, merchant: "listrik", lang: "id",
				at: time.Date(2026, time.September, 5, 12, 0, 0, 0, jakarta),
			}},
		},
		{
			name:  "explicit future day falls back to last month",
			input: "tgl 28 iuran 75rb",
			lang:  "id",
			drafts: []wantDraft{{
				typ: "expense", amount: 75_000, merchant: "iuran", lang: "id",
				at: time.Date(2026, time.August, 28, 12, 0, 0, 0, jakarta),
			}},
		},
		{
			name:   "debt source stays in merchant",
			input:  "sepatu 800k spaylater",
			lang:   "id",
			drafts: []wantDraft{{typ: "expense", amount: 800_000, merchant: "sepatu spaylater", lang: "id"}},
		},
		{
			name:   "installment is a plain expense",
			input:  "cicilan hp 500k",
			lang:   "id",
			drafts: []wantDraft{{typ: "expense", amount: 500_000, merchant: "cicilan hp", lang: "id"}},
		},
		{
			name:  "refund names its merchant",
			input: "refund gojek 30rb",
			// "refund" is a loanword in Indonesian, so it never flips the language.
			lang:   "id",
			drafts: []wantDraft{{typ: "refund", amount: 30_000, merchant: "gojek", lang: "id"}},
		},
		{
			name:   "no amount holds",
			input:  "beli sepatu mahal banget",
			lang:   "id",
			drafts: []wantDraft{{typ: "expense", amount: 0, merchant: "sepatu mahal banget", hold: "no_amount", lang: "id"}},
		},
		{
			name:   "two numbers without roles hold",
			input:  "kopi 25k bayar 50k",
			lang:   "id",
			drafts: []wantDraft{{typ: "expense", amount: 0, merchant: "kopi", hold: "ambiguous_amounts", lang: "id"}},
		},
		{
			name:   "pay and change resolve to the net amount",
			input:  "bayar 50k kembali 25k",
			lang:   "id",
			drafts: []wantDraft{{typ: "expense", amount: 25_000, merchant: "", lang: "id"}},
		},
		{
			name:   "change larger than payment holds",
			input:  "bayar 25k kembali 50k",
			lang:   "id",
			drafts: []wantDraft{{typ: "expense", amount: 0, merchant: "", hold: "ambiguous_amounts", lang: "id"}},
		},
		{
			name:  "transfer without destination holds",
			input: "transfer 500k",
			// "transfer" is a loanword, not an English marker.
			lang:   "id",
			drafts: []wantDraft{{typ: "transfer", amount: 500_000, merchant: "", hold: "no_destination", lang: "id"}},
		},
		{
			name:   "transfer with both ends",
			input:  "pindah 1jt dari bca ke gopay",
			lang:   "id",
			drafts: []wantDraft{{typ: "transfer", amount: 1_000_000, merchant: "", froms: []string{"bca"}, dest: "gopay", lang: "id"}},
		},
		{
			name:   "english segment",
			input:  "lunch 30k with gopay",
			lang:   "en",
			drafts: []wantDraft{{typ: "expense", amount: 30_000, merchant: "lunch", pays: []string{"gopay"}, lang: "en"}},
		},
		{
			name:  "english backdate",
			input: "yesterday coffee 25k",
			lang:  "en",
			drafts: []wantDraft{{
				typ: "expense", amount: 25_000, merchant: "coffee", lang: "en",
				at: time.Date(2026, time.September, 14, 12, 0, 0, 0, jakarta),
			}},
		},
		{
			name:   "grouped rupiah",
			input:  "sewa Rp1.250.000",
			lang:   "id",
			drafts: []wantDraft{{typ: "expense", amount: 1_250_000, merchant: "sewa", lang: "id"}},
		},
		{
			name:   "spelled suffix",
			input:  "donasi 500 ribu",
			lang:   "id",
			drafts: []wantDraft{{typ: "expense", amount: 500_000, merchant: "donasi", lang: "id"}},
		},
		{
			name:   "dan separator",
			input:  "kopi 25k dan gojek 18k",
			lang:   "id",
			drafts: []wantDraft{{typ: "expense", amount: 25_000, merchant: "kopi", lang: "id"}, {typ: "expense", amount: 18_000, merchant: "gojek", lang: "id"}},
		},
		{
			// A category name is not two transactions: the conjunction only
			// splits when both halves carry their own amount.
			name:   "ampersand without amounts stays one draft",
			input:  "makanan & minuman",
			lang:   "id",
			drafts: []wantDraft{{typ: "expense", amount: 0, merchant: "makanan & minuman", hold: "no_amount", lang: "id"}},
		},
		{
			name:   "ampersand with one amount stays one draft",
			input:  "kopi & roti 25k",
			lang:   "id",
			drafts: []wantDraft{{typ: "expense", amount: 25_000, merchant: "kopi & roti", lang: "id"}},
		},
		{
			name:   "referential correction stays one draft",
			input:  "itu kategori makanan & minuman",
			lang:   "id",
			drafts: []wantDraft{{typ: "expense", amount: 0, merchant: "itu kategori makanan & minuman", hold: "no_amount", lang: "id"}},
		},
		{
			// Both "dari" clauses survive: the parser does not know which
			// one is a Source, and "terus" separates transactions.
			name:  "every from clause is kept and terus separates",
			input: "tissu 20k dari alfamart dari bri, terus cimol 10k dari dana",
			lang:  "id",
			drafts: []wantDraft{
				{typ: "expense", amount: 20_000, merchant: "tissu", froms: []string{"alfamart", "bri"}, lang: "id"},
				{typ: "expense", amount: 10_000, merchant: "cimol", froms: []string{"dana"}, lang: "id"},
			},
		},
		{
			name:   "place and pay clauses are separate",
			input:  "beli tissu 20k di alfamart pakai bri",
			lang:   "id",
			drafts: []wantDraft{{typ: "expense", amount: 20_000, merchant: "tissu", pays: []string{"bri"}, places: []string{"alfamart"}, lang: "id"}},
		},
		{
			name:  "lalu separates transactions",
			input: "kopi 25k lalu gojek 18k",
			lang:  "id",
			drafts: []wantDraft{
				{typ: "expense", amount: 25_000, merchant: "kopi", lang: "id"},
				{typ: "expense", amount: 18_000, merchant: "gojek", lang: "id"},
			},
		},
		{
			// A refund's "dari" names the shop, and the harness reads it
			// as a place: the parser still only collects the clause.
			name:   "refund from clause is collected",
			input:  "kembalian dari gojek 18k",
			lang:   "id",
			drafts: []wantDraft{{typ: "refund", amount: 18_000, merchant: "", froms: []string{"gojek"}, lang: "id"}},
		},
		{
			// The place is the whole clause: "ayam balado" is one warung,
			// and keeping only "ayam" used to leave "balado" glued to the
			// merchant.
			name:   "place clause runs to the end of the segment",
			input:  "traktir makan temen 133k di ayam balado",
			lang:   "id",
			drafts: []wantDraft{{typ: "expense", amount: 133_000, merchant: "traktir makan temen", places: []string{"ayam balado"}, lang: "id"}},
		},
		{
			// The pay clause is consumed before the place, so the place
			// still stops where the payment starts.
			name:   "multi-word place with a pay clause after it",
			input:  "makan siang 25k di warteg bu tini pake bca",
			lang:   "id",
			drafts: []wantDraft{{typ: "expense", amount: 25_000, merchant: "makan siang", pays: []string{"bca"}, places: []string{"warteg bu tini"}, lang: "id"}},
		},
		{name: "batal with index", input: "batal 2", cmd: "batal", batalN: 2, lang: "id", noDraft: true},
		{name: "batal whole batch", input: "batal", cmd: "batal", lang: "id", noDraft: true},
		{name: "undo is batal", input: "undo", cmd: "batal", lang: "en", noDraft: true},
		{name: "weekly recap", input: "rekap minggu ini", cmd: "rekap_minggu", lang: "id", noDraft: true},
		{name: "daily recap", input: "rekap", cmd: "rekap_hari", lang: "id", noDraft: true},
		{name: "yes", input: "ya", cmd: "ya", lang: "id", noDraft: true},
		{name: "yes english", input: "yes", cmd: "ya", lang: "en", noDraft: true},
		{name: "no", input: "gak", cmd: "tidak", lang: "id", noDraft: true},
		{name: "trailing punctuation still a command", input: "Ya!", cmd: "ya", lang: "id", noDraft: true},
		{name: "empty message", input: "   ", lang: "id", noDraft: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parse.Parse(tc.input, now)

			assert.Equal(t, tc.cmd, got.Command, "command")
			assert.Equal(t, tc.batalN, got.BatalIndex, "batal index")
			assert.Equal(t, tc.lang, got.Lang, "message language")
			if tc.noDraft {
				assert.Empty(t, got.Drafts, "commands never produce drafts")
				return
			}

			require.Len(t, got.Drafts, len(tc.drafts))
			for i, want := range tc.drafts {
				d := got.Drafts[i]
				assert.Equal(t, want.typ, d.Type, "draft %d type", i)
				assert.Equal(t, want.amount, d.AmountMinor, "draft %d amount", i)
				assert.Equal(t, want.merchant, d.Merchant, "draft %d merchant", i)
				assert.Equal(t, want.pays, d.Pays, "draft %d pay clauses", i)
				assert.Equal(t, want.froms, d.Froms, "draft %d from clauses", i)
				assert.Equal(t, want.places, d.Places, "draft %d place clauses", i)
				assert.Equal(t, want.dest, d.DestHint, "draft %d dest hint", i)
				assert.Equal(t, want.lang, d.Lang, "draft %d language", i)

				if want.hold == "" {
					assert.Nil(t, d.Hold, "draft %d unexpected hold", i)
				} else {
					require.NotNil(t, d.Hold, "draft %d expected hold %s", i, want.hold)
					assert.Equal(t, want.hold, d.Hold.Code, "draft %d hold code", i)
					assert.Equal(t, questionFor(want.hold), d.Hold.Question, "draft %d hold question", i)
				}

				wantAt := want.at
				if wantAt.IsZero() {
					wantAt = now
				}
				assert.True(t, wantAt.Equal(d.OccurredAt), "draft %d occurred_at: want %s got %s", i, wantAt, d.OccurredAt)
				assert.Equal(t, now.Location(), d.OccurredAt.Location(), "draft %d keeps the caller's location", i)
			}
		})
	}
}

func questionFor(code string) string {
	switch code {
	case parse.HoldNoAmount:
		return "q_amount"
	case parse.HoldAmbiguousAmounts:
		return "q_amount_roles"
	case parse.HoldNoDestination:
		return "q_destination"
	}
	return ""
}

func TestParsePhysicalLineBatchEquivalenceAndFooter(t *testing.T) {
	inputs := []string{
		"sarapan bubur ayam 16k\n\ncilok 10k\n\nDari BRI",
		"sarapan bubur ayam 16k\r\n\r\ncilok 10k\r\n\r\nDari BRI",
	}
	for _, input := range inputs {
		got := parse.Parse(input, now)
		require.Len(t, got.Drafts, 2)
		assert.Equal(t, "bri", got.SourceFooter)
		assert.Equal(t, int64(16_000), got.Drafts[0].AmountMinor)
		assert.Equal(t, int64(10_000), got.Drafts[1].AmountMinor)
		assert.Equal(t, "sarapan bubur ayam", got.Drafts[0].Merchant)
		assert.Equal(t, "cilok", got.Drafts[1].Merchant)
	}
}

func TestParseFooterPunctuationAndDateSuffix(t *testing.T) {
	for _, input := range []string{
		"kopi 10k\nteh 5k\nDari: BRI",
		"kopi 10k\nteh 5k\nDari:BRI",
		"kopi 10k\nteh 5k\nDari : BRI",
		"kopi 10k\nteh 5k\nPakai, BRI",
		"kopi 10k\nteh 5k\nPakai,BRI",
	} {
		got := parse.Parse(input, now)
		require.Len(t, got.Drafts, 2)
		assert.Equal(t, "bri", got.SourceFooter)
	}

	got := parse.Parse("kopi 10k\nteh 5k\nkemarin", now)
	assert.Empty(t, got.SourceFooter)
	require.Len(t, got.Drafts, 1)
	assert.Equal(t, now.AddDate(0, 0, -1).Format("2006-01-02"), got.Drafts[0].OccurredAt.Format("2006-01-02"))
}


// TestLooksReferential covers the routing predicate: anything that
// talks about an earlier transaction must reach the intent router
// instead of the regex capture path.
func TestLooksReferential(t *testing.T) {
	tests := map[string]bool{
		"itu kategori makanan":      true,
		"ganti jadi 30k":            true,
		"tadi salah sumbernya":      true,
		"that was the wrong one":    true,
		"actually it was gopay":     true,
		"kopi 25k":                  false,
		"makan siang 30k di warung": false,
		"rekap":                     false,
		"":                          false,
	}
	for input, want := range tests {
		assert.Equal(t, want, parse.LooksReferential(input), "LooksReferential(%q)", input)
	}
}

func TestHasMoneyToken(t *testing.T) {
	tests := map[string]bool{
		"kopi 25k":             true,
		"sewa Rp1.250.000":     true,
		"donasi 500 ribu":      true,
		"itu kategori makanan": false,
		"halo":                 false,
		"":                     false,
	}
	for input, want := range tests {
		assert.Equal(t, want, parse.HasMoneyToken(input), "HasMoneyToken(%q)", input)
	}
}

// TestExtractTime covers the date grammar a chat follow-up leans on:
// an explicit calendar date beats a relative one in the same sentence,
// the hour is read from whatever is left, and a message with no time in
// it reports found=false so the caller keeps its own clock.
func TestExtractTime(t *testing.T) {
	// The capture that motivated the grammar happened on the 18th.
	capture := time.Date(2026, time.September, 18, 9, 0, 0, 0, jakarta)
	day := func(y int, m time.Month, d, h, min int) time.Time {
		return time.Date(y, m, d, h, min, 0, 0, jakarta)
	}

	tests := []struct {
		name  string
		input string
		now   time.Time
		want  time.Time
		rest  string
		found bool
	}{
		{
			name:  "explicit date beats the relative one in the same sentence",
			input: "dari bri, itu kemaren tanggal 17", now: capture,
			want: day(2026, time.September, 17, 12, 0), rest: "dari bri, itu", found: true,
		},
		{name: "yesterday evening", input: "kemarin jam 8 malam", now: capture, want: day(2026, time.September, 17, 20, 0), found: true},
		{name: "yesterday misspelled", input: "kemaren", now: capture, want: day(2026, time.September, 17, 12, 0), found: true},
		{name: "tomorrow is parsed, not refused here", input: "besok", now: capture, want: day(2026, time.September, 19, 12, 0), found: true},
		{name: "day after tomorrow", input: "lusa", now: capture, want: day(2026, time.September, 20, 12, 0), found: true},
		{name: "lusa kemarin is two days back", input: "lusa kemarin", now: capture, want: day(2026, time.September, 16, 12, 0), found: true},
		{name: "n days ago", input: "3 hari yang lalu", now: capture, want: day(2026, time.September, 15, 12, 0), found: true},
		{name: "last week", input: "minggu lalu", now: capture, want: day(2026, time.September, 11, 12, 0), found: true},
		{name: "day and month name", input: "17 agustus", now: capture, want: day(2026, time.August, 17, 12, 0), found: true},
		{name: "a month name still ahead falls back a year", input: "25 desember", now: capture, want: day(2025, time.December, 25, 12, 0), found: true},
		{name: "dd/mm", input: "17/09", now: capture, want: day(2026, time.September, 17, 12, 0), found: true},
		{name: "dd/mm/yy", input: "17-09-25", now: capture, want: day(2025, time.September, 17, 12, 0), found: true},
		{name: "english ordinal", input: "on the 3rd", now: capture, want: day(2026, time.September, 3, 12, 0), found: true},
		{name: "this morning", input: "tadi pagi", now: capture, want: day(2026, time.September, 18, 8, 0), found: true},
		{name: "bare hour with minutes", input: "jam 14.30", now: capture, want: day(2026, time.September, 18, 14, 30), found: true},
		{name: "impossible hour is ignored", input: "jam 99", now: capture, want: capture, found: false},
		{
			name:  "no time at all leaves the text alone",
			input: "kopi 25k", now: capture, want: capture, rest: "kopi 25k", found: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rest, when, found := parse.ExtractTime(tc.input, tc.now)
			assert.Equal(t, tc.found, found, "found")
			assert.True(t, tc.want.Equal(when), "want %s got %s", tc.want, when)
			if tc.rest != "" {
				assert.Equal(t, tc.rest, rest, "remainder")
			}
		})
	}
}
