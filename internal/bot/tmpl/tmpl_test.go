package tmpl_test

import (
	"strings"
	"testing"

	"github.com/glennprays/catet-backend/internal/bot/tmpl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type manyItem struct {
	N                                        int
	Amount, Merchant, Note, Category, Source string
}

type batalItem struct {
	N                int
	Amount, Merchant string
}

type topItem struct {
	Name, Amount string
}

type balanceItem struct {
	Source, Balance string
}

type receiptLine struct {
	Name, Amount, Category string
}

type paymentPart struct {
	Source, Amount string
}

// fixtures pairs every guaranteed template with populated data and the
// money strings that must survive rendering verbatim — the bot never
// reformats an amount it was handed.
var fixtures = map[string]struct {
	data      any
	wantMoney []string
}{
	// A confirmation carries only the amount it recorded: the balance
	// lives in the rituals below, never in an unsolicited reply.
	"recorded_one": {
		data: map[string]any{
			"Amount": "Rp25.000", "Merchant": "kopi", "Note": "", "Category": "Makanan & Minuman",
			"Source": "Cash", "Backdated": true, "Date": "14 Sep",
		},
		wantMoney: []string{"Rp25.000"},
	},
	"recorded_income": {
		data:      map[string]any{"Amount": "Rp8.000.000", "Merchant": "gajian", "Source": "BCA"},
		wantMoney: []string{"Rp8.000.000"},
	},
	"recorded_transfer": {
		data:      map[string]any{"Amount": "Rp500.000", "Source": "BCA", "Destination": "GoPay"},
		wantMoney: []string{"Rp500.000"},
	},
	"recorded_many": {
		data: map[string]any{
			"Items": []manyItem{
				{N: 1, Amount: "Rp25.000", Merchant: "kopi", Note: "", Category: "Makanan & Minuman", Source: "Cash"},
				{N: 2, Amount: "Rp18.000", Merchant: "gojek", Note: "ke kantor", Category: "Transport", Source: "GoPay"},
			},
		},
		wantMoney: []string{"Rp25.000", "Rp18.000"},
	},
	"uncategorized":     {data: nil},
	"hold_saved":        {data: map[string]any{"Summary": "Pindah Rp500.000", "Question": "Dipindah ke mana? BCA / GoPay"}, wantMoney: []string{"Rp500.000"}},
	"hold_to_dashboard": {data: nil},
	"q_amount":          {data: nil},
	"q_amount_roles":    {data: nil},
	"q_source":          {data: map[string]any{"Sources": []string{"BCA", "GoPay", "Cash"}}},
	"q_destination":     {data: map[string]any{"Sources": []string{"BCA", "GoPay", "Cash"}}},
	"q_refund_target": {
		data:      map[string]any{"Merchant": "gojek", "Amount": "Rp18.000", "Date": "14 Sep"},
		wantMoney: []string{"Rp18.000"},
	},
	"refund_done": {
		data:      map[string]any{"Merchant": "gojek", "Amount": "Rp18.000", "Source": "GoPay"},
		wantMoney: []string{"Rp18.000"},
	},
	"refund_no_target": {data: map[string]any{"Merchant": "gojek"}},
	"batal_done": {
		data: map[string]any{
			"Items": []batalItem{
				{N: 1, Amount: "Rp25.000", Merchant: "kopi"},
			},
		},
		wantMoney: []string{"Rp25.000"},
	},
	"batal_nothing":    {data: nil},
	"batal_bad_index":  {data: map[string]any{"N": 5, "Count": 2}},
	"discarded":        {data: nil},
	"no_open_question": {data: nil},
	"open_question_reminder": {
		data:      map[string]any{"Summary": "Struk Indomaret · 14 Sep · 2 item · Rp45.000"},
		wantMoney: []string{"Rp45.000"},
	},
	"unknown": {data: nil},
	"corrected": {
		data: map[string]any{
			"Amount": "Rp25.000", "Merchant": "ayam crispi", "Category": "Makanan & Minuman",
			"Source": "BCA", "Destination": "", "Type": "expense",
		},
		wantMoney: []string{"Rp25.000"},
	},
	"correct_nothing": {data: nil},
	"correct_failed":  {data: map[string]any{"Field": "kategori"}},
	"balance_one": {
		data:      map[string]any{"Source": "BCA", "Balance": "Rp1.250.000"},
		wantMoney: []string{"Rp1.250.000"},
	},
	"balance_all": {
		data: map[string]any{
			"Items": []balanceItem{
				{Source: "BCA", Balance: "Rp1.250.000"},
				{Source: "GoPay", Balance: "Rp75.000"},
			},
			"Sts": "Rp900.000",
		},
		wantMoney: []string{"Rp1.250.000", "Rp75.000", "Rp900.000"},
	},
	"unsupported_media": {data: nil},
	"photo_unreadable":  {data: map[string]any{"Issue": "blur"}},
	"photo_no_total":    {data: nil},
	"photo_not_receipt": {data: nil},
	"photo_vision_down": {data: nil},
	"photo_statement":   {data: nil},
	"photo_currency":    {data: map[string]any{"Currency": "MYR"}},
	"photo_too_large":   {data: nil},
	"photo_duplicate": {
		data:      map[string]any{"Summary": "Struk Indomaret · 14 Sep · 3 item · Rp45.000"},
		wantMoney: []string{"Rp45.000"},
	},
	"photo_llm_off": {data: nil},
	"receipt_summary": {
		data: map[string]any{
			"Payment": false, "Merchant": "Indomaret", "Date": "14 Sep", "N": 3,
			"Total": "Rp45.000", "Duplicate": false, "Inconsistent": false,
		},
		wantMoney: []string{"Rp45.000"},
	},
	"receipt_hold": {
		data: map[string]any{
			"Summary": "Struk Indomaret · 14 Sep · 2 item · Rp45.000",
			"Items": []receiptLine{
				{Name: "Aqua 600ml", Amount: "Rp5.000", Category: "Makanan & Minuman"},
				{Name: "Indomie goreng", Amount: "Rp40.000"},
			},
			"More": 0, "Source": "BCA", "AskSource": false, "Basis": "printed",
			"Sources": []string{"BCA", "GoPay", "Cash"},
		},
		wantMoney: []string{"Rp45.000", "Rp5.000", "Rp40.000"},
	},
	"receipt_recorded": {
		data:      map[string]any{"N": 2, "Merchant": "Indomaret", "Total": "Rp45.000", "Source": "BCA"},
		wantMoney: []string{"Rp45.000"},
	},
	"receipt_updated": {data: map[string]any{"Change": "dari BCA · kategori Makanan & Minuman"}},
	"receipt_need_source": {
		data: map[string]any{"Sources": []string{"BCA", "GoPay", "Cash"}},
	},
	"receipt_default_learned": {data: map[string]any{"Source": "BCA"}},
	"receipt_split_mismatch": {
		data:      map[string]any{"Parts": "Rp20.000 + Rp15.000", "Total": "Rp38.000"},
		wantMoney: []string{"Rp20.000", "Rp15.000", "Rp38.000"},
	},
	"receipt_split_unknown_source": {
		data: map[string]any{"Sources": []string{"BCA", "GoPay", "Cash"}},
	},
	"receipt_change_source": {data: map[string]any{"Source": "BCA"}},
	"receipt_change_split": {
		data: map[string]any{"Payments": []paymentPart{
			{Source: "Cash", Amount: "Rp20.000"},
			{Source: "BCA", Amount: "Rp18.000"},
		}},
		wantMoney: []string{"Rp20.000", "Rp18.000"},
	},
	"receipt_change_category": {data: map[string]any{"Category": "Makanan & Minuman"}},
	"receipt_change_merchant": {data: map[string]any{"Merchant": "Indomaret"}},
	"receipt_change_date":     {data: map[string]any{"Date": "14 Sep"}},
	"rekap_hari": {
		data: map[string]any{
			"Spent": "Rp143.000", "Sts": "Rp1.250.000", "ReservedTomorrow": "Rp75.000",
			"Uncategorized": 3, "Verdict": "Santai, pengeluaran hari ini masih wajar.",
		},
		wantMoney: []string{"Rp143.000", "Rp1.250.000", "Rp75.000"},
	},
	"rekap_minggu": {
		data: map[string]any{
			"Spent": "Rp1.043.000", "Delta": "+Rp120.000",
			"AnomalyDays":   []string{"Sen 8 Sep", "Kam 11 Sep"},
			"Uncategorized": 2,
			"Top": []topItem{
				{Name: "gojek", Amount: "Rp180.000"},
				{Name: "indomaret", Amount: "Rp95.000"},
			},
			"Verdict": "Pengeluaran transport naik minggu ini.",
		},
		wantMoney: []string{"Rp1.043.000", "+Rp120.000", "Rp180.000", "Rp95.000"},
	},
	// A reminder never states an amount (ADR-0006), so this fixture has
	// no wantMoney and the test below asserts the absence instead.
	"bill_reminder": {
		data: map[string]any{
			"Overdue": []string{"Indihome"}, "Today": []string{"Listrik"},
			"Tomorrow": []string{"Spotify"}, "InThree": []string{"Kos"},
		},
	},
	"lunas_none":   {data: nil},
	"lunas_choose": {data: map[string]any{"Items": []string{"1. Listrik · 14 Sep", "2. Indihome · 15 Sep"}}},
	"lunas_amount": {data: map[string]any{"Name": "Listrik"}},
	"lunas_source": {
		data: map[string]any{"Name": "Listrik", "Sources": []string{"BCA", "GoPay", "Cash"}},
	},
	"lunas_done": {
		data:      map[string]any{"Name": "Listrik", "Amount": "Rp230.000", "Next": "14 Okt"},
		wantMoney: []string{"Rp230.000"},
	},
	"lunas_completed": {data: nil},

	// The invite link is the whole message: it must survive rendering
	// exactly, query string included.
	"invite_sent":   {data: map[string]any{"URL": "http://localhost:3001/register?invite=abc123"}},
	"error_generic": {data: nil},
}

func TestRenderEveryNameInEveryLanguage(t *testing.T) {
	for _, name := range tmpl.Names() {
		fx, ok := fixtures[name]
		require.True(t, ok, "no fixture for guaranteed template %q", name)
		for _, lang := range []string{"id", "en"} {
			out := tmpl.Render(lang, name, fx.data)
			assert.NotEmpty(t, out, "%s/%s rendered empty", lang, name)
			assert.NotContains(t, out, "<no value>", "%s/%s has an unfilled slot", lang, name)
			assert.NotContains(t, out, "{{", "%s/%s leaked template syntax", lang, name)
			assert.NotContains(t, out, "%!", "%s/%s leaked a format artifact", lang, name)
			assert.Equal(t, strings.TrimSpace(out), out, "%s/%s has stray whitespace", lang, name)
			for _, m := range fx.wantMoney {
				assert.Contains(t, out, m, "%s/%s dropped money %q", lang, name, m)
			}
		}
	}
}

func TestRenderIndonesianText(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"recorded_one", "Sip, tercatat: Rp25.000 · kopi · Makanan & Minuman · Cash · 14 Sep"},
		{"recorded_income", "Sip, masuk Rp8.000.000 · gajian · ke BCA"},
		{"recorded_transfer", "Sip, pindah Rp500.000 · BCA → GoPay"},
		{"recorded_many", "Sip, 2 transaksi tercatat:\n1. Rp25.000 · kopi · Makanan & Minuman · Cash\n2. Rp18.000 · gojek · ke kantor · Transport · GoPay\nKalau ada yang salah, balas \"batal\" untuk hapus semua atau \"batal 2\" untuk nomor 2."},
		{"uncategorized", "belum ada kategori"},
		{"hold_saved", "Aku tahan dulu: Pindah Rp500.000\nDipindah ke mana? BCA / GoPay"},
		{"hold_to_dashboard", "Aku simpan sebagai draft dulu, lengkapi di dashboard ya."},
		{"q_amount", "Nominalnya berapa?"},
		{"q_source", "Bayar pakai sumber mana? BCA / GoPay / Cash"},
		{"q_destination", "Dipindah ke mana? BCA / GoPay / Cash"},
		{"q_refund_target", "Refund untuk gojek Rp18.000 (14 Sep), betul? Balas ya/tidak."},
		{"refund_done", "Oke, refund gojek Rp18.000 sudah kukembalikan ke GoPay."},
		{"refund_no_target", "Aku nggak nemu transaksi gojek dalam 30 hari terakhir. Coba cek di dashboard ya."},
		{"photo_unreadable", "Fotonya buram, aku nggak yakin sama angkanya. Coba foto ulang lebih dekat, rata, dan terang ya."},
		{"photo_currency", "Mata uang MYR belum kudukung, cuma Rupiah dulu ya."},
		{"photo_duplicate", "Foto ini sudah pernah kamu kirim: Struk Indomaret · 14 Sep · 3 item · Rp45.000"},
		{"receipt_summary", "Struk Indomaret · 14 Sep · 3 item · Rp45.000"},
		{"receipt_hold", "Struk Indomaret · 14 Sep · 2 item · Rp45.000\n• Aqua 600ml Rp5.000 · Makanan & Minuman\n• Indomie goreng Rp40.000\nBayar dari BCA.\nBalas \"ya\" kalau sudah pas, \"tidak\" untuk buang. Mau ganti sumber atau kategori? Tinggal bilang."},
		{"receipt_recorded", "Sip, 2 baris dari Indomaret tercatat · Rp45.000 dari BCA. Kalau salah, ketik \"batal\" ya."},
		{"receipt_updated", "Oke, dari BCA · kategori Makanan & Minuman. Sudah pas? Balas ya/tidak."},
		{"receipt_need_source", "Bayar dari mana dulu? BCA / GoPay / Cash"},
		{"receipt_default_learned", "Struk berikutnya kuanggap dari BCA kecuali tertulis lain — bisa kamu ubah di Settings."},
		{"receipt_split_mismatch", "Hmm, jumlahnya nggak pas: Rp20.000 + Rp15.000 ≠ Rp38.000. Coba lagi ya."},
		{"receipt_split_unknown_source", "Sumbernya nggak kukenal. Pilihannya: BCA / GoPay / Cash"},
		{"receipt_change_source", "dari BCA"},
		{"receipt_change_split", "Cash Rp20.000 · BCA Rp18.000"},
		{"receipt_change_category", "kategori Makanan & Minuman"},
		{"receipt_change_merchant", "merchant Indomaret"},
		{"receipt_change_date", "tanggal 14 Sep"},
		{"open_question_reminder", "Aku masih nunggu jawaban buat: Struk Indomaret · 14 Sep · 2 item · Rp45.000. Balas \"ya\"/\"tidak\", atau sebut sumbernya ya."},
		{"batal_done", "Oke, 1 transaksi kubatalkan:\n1. Rp25.000 · kopi"},
		{"batal_nothing", "Nggak ada yang bisa dibatalkan dari chat. Hapus lewat dashboard ya."},
		{"batal_bad_index", "Nomor 5 nggak ada — batch terakhir cuma 2 item."},
		{"discarded", "Oke, kubuang. Nggak ada yang ditulis."},
		{"no_open_question", "Nggak ada yang lagi nunggu jawaban."},
		{"corrected", "Oke, kuperbaiki: Rp25.000 · ayam crispi · Makanan & Minuman · BCA"},
		{"correct_nothing", "Belum ada transaksi dari chat yang bisa kuubah. Ubah lewat dashboard ya."},
		{"correct_failed", "kategori untuk transaksi itu nggak bisa kuubah dari chat. Ubah lewat dashboard ya."},
		{"balance_one", "BCA: Rp1.250.000"},
		{"balance_all", "BCA: Rp1.250.000\nGoPay: Rp75.000\nSafe-to-Spend Rp900.000"},
		{"unsupported_media", "Kirim sebagai foto ya — video, audio, dan dokumen belum bisa kubaca."},
		{"unknown", `Hmm, aku belum nangkep maksudnya. Kalau mau catat pengeluaran, ketik misalnya "kopi 25k pakai gopay"; buat lihat ringkasan, ketik "rekap"; buat koreksi, "itu kategori makanan".`},
		{"rekap_hari", "Hari ini kamu keluar Rp143.000 · Safe-to-Spend Rp1.250.000 · besok reserved Rp75.000 · 3 belum kategori\nSantai, pengeluaran hari ini masih wajar."},
		{"rekap_minggu", "Minggu ini kamu keluar Rp1.043.000 (+Rp120.000 vs minggu lalu)\nHari yang beda dari biasanya: Sen 8 Sep, Kam 11 Sep\n2 belum kategori\nTeratas: gojek Rp180.000 · indomaret Rp95.000\nPengeluaran transport naik minggu ini."},
		{"invite_sent", "Kamu diundang ke catet. Daftar lewat link ini (berlaku 7 hari): http://localhost:3001/register?invite=abc123\nKalau link ini kamu terima lagi, pakai yang terbaru."},
		{"error_generic", "Aduh, ada yang error di sisiku. Coba lagi sebentar ya."},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, tmpl.Render("id", c.name, fixtures[c.name].data), c.name)
	}
}

// A typed "tissu 20k dari alfamart" records the shop as the merchant and
// what was bought as the note; the confirmation shows both.
func TestRecordedOneShowsTheNote(t *testing.T) {
	got := tmpl.Render("id", "recorded_one", map[string]any{
		"Amount": "Rp20.000", "Merchant": "alfamart", "Note": "tissu",
		"Category": "Belanja", "Source": "BRI",
	})
	assert.Equal(t, "Sip, tercatat: Rp20.000 · alfamart · tissu · Belanja · BRI", got)
}

// The basis switch is the only part of the Source line that moves: a
// remembered or default Source is hedged, a model guess is marked, and an
// unresolved Source asks instead of stating.
func TestReceiptHoldWordsTheSourceBasis(t *testing.T) {
	base, ok := fixtures["receipt_hold"].data.(map[string]any)
	require.True(t, ok)
	with := func(key string, value any) map[string]any {
		out := make(map[string]any, len(base))
		for k, v := range base {
			out[k] = v
		}
		out[key] = value
		return out
	}
	assert.Contains(t, tmpl.Render("id", "receipt_hold", with("Basis", "memory")), "Bayar dari BCA (biasanya).")
	assert.Contains(t, tmpl.Render("id", "receipt_hold", with("Basis", "default")), "Bayar dari BCA (biasanya).")
	assert.Contains(t, tmpl.Render("id", "receipt_hold", with("Basis", "guess")), "Bayar dari BCA?")
	assert.Contains(t, tmpl.Render("id", "receipt_hold", with("Basis", "hint")), "Bayar dari BCA.")
	assert.Contains(t, tmpl.Render("en", "receipt_hold", with("Basis", "memory")), "Paid from BCA (usually).")
	assert.Contains(t, tmpl.Render("en", "receipt_hold", with("Basis", "guess")), "Paid from BCA?")

	ask := tmpl.Render("id", "receipt_hold", with("AskSource", true))
	assert.Contains(t, ask, "Bayar dari mana? BCA / GoPay / Cash")
	assert.NotContains(t, ask, "Bayar dari BCA")
}

func TestUnknownLangFallsBackToIndonesian(t *testing.T) {
	data := fixtures["recorded_one"].data
	assert.Equal(t, tmpl.Render("id", "recorded_one", data), tmpl.Render("fr", "recorded_one", data))
	assert.Equal(t, "Nominalnya berapa?", tmpl.Render("fr", "q_amount", nil))
	assert.NotEqual(t, tmpl.Render("en", "q_amount", nil), tmpl.Render("fr", "q_amount", nil))
}

func TestRekapMingguWithoutAnomalyDays(t *testing.T) {
	data := map[string]any{
		"Spent": "Rp1.043.000", "Delta": "+Rp120.000",
		"AnomalyDays":   []string{},
		"Uncategorized": 0,
		"Top":           []topItem{{Name: "gojek", Amount: "Rp180.000"}},
		"Verdict":       "",
	}
	id := tmpl.Render("id", "rekap_minggu", data)
	assert.Contains(t, id, "Hari yang beda dari biasanya: tidak ada")
	assert.False(t, strings.HasSuffix(id, "\n"), "verdict slot left a trailing newline: %q", id)
	assert.Contains(t, tmpl.Render("en", "rekap_minggu", data), "Days that stood out: none")
}

func TestRenderUnknownNameIsEmpty(t *testing.T) {
	assert.Empty(t, tmpl.Render("id", "not_a_template", nil))
}

func TestRenderExecutionErrorIsEmpty(t *testing.T) {
	// A struct missing the fields the template needs must not leak the
	// Go error text into a WhatsApp reply.
	assert.Empty(t, tmpl.Render("id", "recorded_one", struct{ Amount string }{"Rp1"}))
}

func TestNamesIsACopy(t *testing.T) {
	first := tmpl.Names()
	require.NotEmpty(t, first)
	first[0] = "mutated"
	assert.NotEqual(t, "mutated", tmpl.Names()[0])
}

// A bill reminder is the one message the bot sends unprompted, so its
// content is load-bearing: names and due days only. An amount on a lock
// screen would disclose the ledger to whoever is holding the phone.
func TestBillReminderNeverStatesAnAmount(t *testing.T) {
	data := map[string]any{
		"Overdue": []string{"Indihome"}, "Today": []string{"Listrik"},
		"Tomorrow": []string{"Spotify"}, "InThree": []string{"Kos"},
	}
	for _, lang := range []string{"id", "en"} {
		out := tmpl.Render(lang, "bill_reminder", data)
		assert.NotContains(t, out, "Rp", "%s/bill_reminder must never carry money", lang)
		for _, name := range []string{"Indihome", "Listrik", "Spotify", "Kos"} {
			assert.Contains(t, out, name, "%s/bill_reminder dropped %q", lang, name)
		}
	}
	assert.Contains(t, tmpl.Render("id", "bill_reminder", map[string]any{
		"Today": []string{"Listrik"},
	}), "Hari ini: Listrik")
	assert.NotContains(t, tmpl.Render("id", "bill_reminder", map[string]any{
		"Today": []string{"Listrik"},
	}), "Terlambat", "empty sections are omitted, not rendered blank")
}
