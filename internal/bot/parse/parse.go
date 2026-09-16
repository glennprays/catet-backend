// Package parse turns one inbound WhatsApp message into transaction
// drafts using deterministic rules only. Every money fact the bot
// records originates here: the LLM may label a category or resolve a
// source alias, but it never invents an amount, a date or a direction
// (ADR-0003). When the rules cannot decide, the draft carries a
// HoldReason instead of a guess — the harness parks it and asks exactly
// one question.
//
// The package is pure: no database, no network, no clock of its own.
// The caller passes `now` already in Asia/Jakarta and every derived
// timestamp keeps that location.
package parse

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/glennprays/catet-backend/internal/money"
)

// Draft types. A refund is a linked reversal, not a negative expense.
const (
	TypeExpense  = "expense"
	TypeIncome   = "income"
	TypeTransfer = "transfer"
	TypeRefund   = "refund"
)

// Hold codes. The harness stores the code; the question is a template
// name because user-visible copy lives only in internal/bot/tmpl.
const (
	HoldNoAmount         = "no_amount"
	HoldAmbiguousAmounts = "ambiguous_amounts"
	HoldNoDestination    = "no_destination"
)

// Commands recognised when they are the whole message.
const (
	CmdBatal       = "batal"
	CmdRekapHari   = "rekap_hari"
	CmdRekapMinggu = "rekap_minggu"
	CmdYa          = "ya"
	CmdTidak       = "tidak"
	CmdLunas       = "lunas"
)

// Supported languages.
const (
	LangID = "id"
	LangEN = "en"
)

// HoldReason is the parser refusing to guess. Question is a template
// name rendered later by internal/bot/tmpl, never literal copy.
type HoldReason struct {
	Code     string
	Question string
}

// Draft is one candidate transaction extracted from a message segment.
// Source and destination are only *hints* — resolving them against the
// Source whitelist is the harness's job.
type Draft struct {
	Type        string
	AmountMinor int64
	Merchant    string
	// Pays are the clauses that can only mean a Source ("pakai bca").
	// Froms are "dari X": a Source when the harness matches X, a place
	// otherwise. Places are "di X": always where the money was spent.
	// All lowercase, punctuation-trimmed, in message order; the parser
	// never decides which From is which — it does not know the Sources.
	Pays       []string
	Froms      []string
	Places     []string
	DestHint   string
	OccurredAt time.Time
	Lang       string
	Hold       *HoldReason
}

// Parsed is the whole message: either one command or a batch of drafts.
type Parsed struct {
	Drafts     []Draft
	Command    string
	BatalIndex int
	Lang       string
	// BillQuery and Amount carry the `lunas` operands: which bill (a
	// name fragment, "" for "whatever is due") and how much, when the
	// user said it.
	BillQuery string
	Amount    *int64
}

var (
	spaceRe = regexp.MustCompile(`\s+`)

	// Hard segment separators: the message "kopi 25k, gojek 18k" is two
	// captures, not one merchant with two numbers. Punctuation always
	// splits; a conjunction only splits when both halves carry their own
	// money token, because "makanan & minuman" is one name.
	segmentRe     = regexp.MustCompile(`[,;]`)
	conjunctionRe = regexp.MustCompile(`\s+(?:dan|terus|lalu|kemudian|abis itu|then|and)\s+|\s+&\s+|\s+\+\s+`)
	leadConjRe    = regexp.MustCompile(`^(?:dan|terus|lalu|kemudian|abis itu|then|and)\s+`)

	batalRe = regexp.MustCompile(`^(?:batal|cancel|undo)(?:\s+(\d+))?$`)

	// "lunas", "lunas 250k", "lunas listrik", "lunas listrik 250k".
	// Everything after the verb is one blob; the trailing money token is
	// peeled off with the same reader the capture path uses, so "lunas
	// indihome 350rb" cannot read the amount differently from "indihome
	// 350rb".
	lunasRe = regexp.MustCompile(`^(?:lunas|paid)(?:\s+(.*?))?$`)

	// The suffix is glued to the number (`25rb`) or separated by spaces
	// (`500 ribu`); keeping \s* inside the optional group stops "50 kopi"
	// from swallowing the space. Longest suffixes first: RE2 alternation
	// is leftmost-first, so "ribu" must be tried before "rb".
	moneyRe = regexp.MustCompile(`\b(?:rp\s*)?\d[\d.,]*(?:\s*(?:ribu|juta|rb|jt|k|m))?\b`)

	payWordRe    = regexp.MustCompile(`\b(?:bayar|pay)\b`)
	changeWordRe = regexp.MustCompile(`\b(?:kembalian|kembali|change)\b`)
	srcRoleRe    = regexp.MustCompile(`\b(?:dari|from)\b`)

	transferRe = regexp.MustCompile(`\b(?:pindah|pindahin|transfer|tf|move)\b`)
	refundRe   = regexp.MustCompile(`\b(?:refund|balikin|dikembalikan|kembalian dari)\b`)
	incomeRe   = regexp.MustCompile(`\b(?:gajian|gaji|masuk|dapat|terima|income|salary|received|bonus|thr|cashback)\b`)

	payHintRe   = regexp.MustCompile(`(?:^|\s)(?:pakai|pake|via|dgn|dengan|with|by)\s+(\S+)`)
	fromHintRe  = regexp.MustCompile(`(?:^|\s)(?:dari|from)\s+(\S+)`)
	placeHintRe = regexp.MustCompile(`(?:^|\s)(?:di|at)\s+(\S+)`)
	destHintRe  = regexp.MustCompile(`(?:^|\s)(?:ke|to)\s+(\S+)`)

	wordRe = regexp.MustCompile(`[a-z]+`)
)

// commands maps an exact (normalised) message to its command.
var commands = map[string]string{
	"rekap hari ini":   CmdRekapHari,
	"rekap":            CmdRekapHari,
	"recap today":      CmdRekapHari,
	"today":            CmdRekapHari,
	"rekap minggu ini": CmdRekapMinggu,
	"rekap minggu":     CmdRekapMinggu,
	"recap week":       CmdRekapMinggu,
	"this week":        CmdRekapMinggu,
	"ya":               CmdYa,
	"y":                CmdYa,
	"yes":              CmdYa,
	"ok":               CmdYa,
	"oke":              CmdYa,
	"sip":              CmdYa,
	"iya":              CmdYa,
	"betul":            CmdYa,
	"tidak":            CmdTidak,
	"tdk":              CmdTidak,
	"ga":               CmdTidak,
	"gak":              CmdTidak,
	"nggak":            CmdTidak,
	"no":               CmdTidak,
	"n":                CmdTidak,
}

// enWords marks a message as English. `transfer` and `refund` are
// deliberately absent: Indonesian uses both loanwords unchanged, so
// counting them would answer an Indonesian user in English.
var enWords = map[string]bool{
	"pay": true, "from": true, "yesterday": true, "lunch": true,
	"dinner": true, "coffee": true, "move": true,
	"to": true, "salary": true, "today": true, "week": true,
	"cancel": true, "undo": true, "yes": true,
	"no": true, "with": true,
}

var enPhrases = []string{"this morning", "last night"}

var idWords = map[string]bool{
	"bayar": true, "dari": true, "kemarin": true, "makan": true,
	"kopi": true, "pindah": true, "gaji": true, "hari": true,
	"minggu": true, "batal": true, "ya": true, "tidak": true,
	"pakai": true, "pake": true, "ke": true, "via": true, "dengan": true,
}

// leadVerbs are dropped from the front of a merchant: they describe the
// act, not the thing bought.
var leadVerbs = map[string]bool{
	"beli": true, "bayar": true, "buy": true, "pay": true,
	"untuk": true, "for": true,
}

var leadTransfer = map[string]bool{
	"pindah": true, "pindahin": true, "transfer": true, "tf": true, "move": true,
}

var leadRefund = map[string]bool{
	"refund": true, "balikin": true, "dikembalikan": true,
	"kembalian": true, "dari": true, "from": true,
}

// Parse reads one message. now must already be in Asia/Jakarta; every
// timestamp Parse returns keeps now's location.
func Parse(text string, now time.Time) Parsed {
	norm := normalize(text)
	if norm == "" {
		return Parsed{Lang: LangID}
	}
	if cmd, idx, ok := matchCommand(norm); ok {
		return Parsed{Command: cmd, BatalIndex: idx, Lang: detectLang(norm)}
	}
	if m := lunasRe.FindStringSubmatch(norm); m != nil {
		query, amount := splitLunasOperands(m[1])
		return Parsed{Command: CmdLunas, BillQuery: query, Amount: amount, Lang: detectLang(norm)}
	}

	out := Parsed{Lang: detectLang(norm)}
	for _, seg := range segmentRe.Split(norm, -1) {
		for _, piece := range splitConjunctions(seg) {
			// A segment may still open with the glue word that a comma
			// already separated: ", terus cimol 10k" records cimol.
			piece = leadConjRe.ReplaceAllString(normalize(piece), "")
			if piece == "" {
				continue
			}
			out.Drafts = append(out.Drafts, parseSegment(piece, now))
		}
	}
	if len(out.Drafts) > 0 {
		out.Lang = out.Drafts[0].Lang
	}
	return out
}

// splitConjunctions breaks "kopi 25k dan gojek 18k" into two captures
// while leaving "makanan & minuman" and "kopi & roti 25k" whole: a
// conjunction only separates transactions when every side names its own
// amount. Without that guard a category name or a compound merchant
// would become two amountless drafts and two junk Hold Queue rows. A
// conjunction that sits inside a time phrase ("2 hari lalu") is glue
// within one date, never a separator.
func splitConjunctions(seg string) []string {
	var cuts [][]int
	protected := timePhraseSpans(seg)
	for _, loc := range conjunctionRe.FindAllStringIndex(seg, -1) {
		if overlapsAny(loc, protected) {
			continue
		}
		cuts = append(cuts, loc)
	}
	if len(cuts) == 0 {
		return []string{seg}
	}
	parts := make([]string, 0, len(cuts)+1)
	prev := 0
	for _, loc := range cuts {
		parts = append(parts, seg[prev:loc[0]])
		prev = loc[1]
	}
	parts = append(parts, seg[prev:])
	for _, p := range parts {
		if !HasMoneyToken(p) {
			return []string{seg}
		}
	}
	return parts
}

// timePhraseSpans reports every span a time rule claims, so the
// conjunction splitter can leave those words alone.
func timePhraseSpans(seg string) []span {
	var out []span
	for _, r := range timeRules {
		for _, loc := range r.re.FindAllStringIndex(seg, -1) {
			out = append(out, span{loc[0], loc[1]})
		}
	}
	return out
}

func overlapsAny(loc []int, spans []span) bool {
	for _, s := range spans {
		if loc[0] < s.end && s.start < loc[1] {
			return true
		}
	}
	return false
}

// HasMoneyToken reports whether the text contains at least one token
// the money grammar accepts. The harness routes on it: a message with
// no money in it can never be a deterministic capture.
func HasMoneyToken(text string) bool {
	seg := normalize(text)
	for _, loc := range moneyRe.FindAllStringIndex(seg, -1) {
		if _, ok := money.ParseToken(seg[loc[0]:loc[1]]); ok {
			return true
		}
	}
	return false
}

// referentialRe marks a message that talks *about* an earlier
// transaction rather than recording a new one ("itu kategori makanan",
// "ganti jadi 30k"). Such a message is never handled by the regex
// capture path — it goes to the intent router, which can see the recent
// batch. False positives are cheap (the router still classifies a
// capture as a capture); false negatives write junk.
var referentialRe = regexp.MustCompile(
	`\b(?:itu|tadi|barusan|ganti|ubah|koreksi|salah|bukan|maksudnya|harusnya|seharusnya|jadi|jadiin|` +
		`that|last|previous|change|fix|correct|actually|meant|instead|wrong)\b`)

// LooksReferential reports whether the message refers to something
// already captured.
func LooksReferential(text string) bool {
	return referentialRe.MatchString(normalize(text))
}

// SourceHint pulls the Source clause out of a free-form message
// ("bayar pake BCA, ya catat" → "bca") and returns what is left once
// that clause is removed. The harness uses the remainder to decide
// whether the rest of the message was only a yes/no: a message it can
// explain word for word needs no model call.
func SourceHint(text string) (hint, rest string) {
	norm := normalize(text)
	if rest, hint = takeHint(norm, payHintRe); hint != "" {
		return trimClause(hint), rest
	}
	rest, hint = takeHint(norm, fromHintRe)
	return trimClause(hint), rest
}

// trimClause drops the punctuation that glues a clause to the rest of a
// sentence: "pake bca, ya" names BCA, not "bca,".
func trimClause(s string) string {
	return strings.Trim(s, ",.;:!?\"'")
}

// CommandOf reports the command one word names ("ya" → CmdYa) and ""
// for anything else. It exists so a caller can test the words of a
// mixed message without re-running the whole grammar.
func CommandOf(word string) string {
	return commands[normalize(word)]
}

// normalize lowercases, folds whitespace and drops sentence-final
// punctuation so the grammar only ever sees canonical text.
func normalize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = spaceRe.ReplaceAllString(s, " ")
	return strings.TrimRight(s, " .!?")
}

func matchCommand(norm string) (string, int, bool) {
	if m := batalRe.FindStringSubmatch(norm); m != nil {
		idx := 0
		if m[1] != "" {
			n, err := strconv.Atoi(m[1])
			if err == nil {
				idx = n
			}
		}
		return CmdBatal, idx, true
	}
	if cmd, ok := commands[norm]; ok {
		return cmd, 0, true
	}
	return "", 0, false
}

// splitLunasOperands peels a trailing money token off the operand blob.
// Only a trailing token counts: a bill named "indihome 3" keeps its
// digits when no amount follows.
func splitLunasOperands(rest string) (string, *int64) {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", nil
	}
	locs := moneyRe.FindAllStringIndex(rest, -1)
	for i := len(locs) - 1; i >= 0; i-- {
		loc := locs[i]
		if strings.TrimSpace(rest[loc[1]:]) != "" {
			break // not trailing
		}
		v, ok := money.ParseToken(rest[loc[0]:loc[1]])
		if !ok {
			break
		}
		return strings.TrimSpace(rest[:loc[0]]), &v
	}
	return rest, nil
}

func parseSegment(seg string, now time.Time) Draft {
	d := Draft{
		Type:       detectType(seg),
		Lang:       detectLang(seg),
		OccurredAt: now,
	}

	// Time first: "tgl 5" and "2 hari lalu" carry digits that would
	// otherwise be read as money.
	rest, when := extractTime(seg, now)
	d.OccurredAt = when

	amount, hold, rest := extractAmount(rest)
	d.AmountMinor = amount
	d.Hold = hold

	rest = d.extractHints(rest)
	d.Merchant = merchantOf(rest, d.Type)

	if d.Type == TypeTransfer && d.DestHint == "" && d.Hold == nil {
		d.Hold = &HoldReason{Code: HoldNoDestination, Question: "q_destination"}
	}
	return d
}

func detectType(seg string) string {
	switch {
	case transferRe.MatchString(seg):
		return TypeTransfer
	case refundRe.MatchString(seg):
		return TypeRefund
	case incomeRe.MatchString(seg):
		return TypeIncome
	default:
		return TypeExpense
	}
}

func detectLang(s string) string {
	en, id := 0, 0
	for _, w := range wordRe.FindAllString(s, -1) {
		if enWords[w] {
			en++
		}
		if idWords[w] {
			id++
		}
	}
	for _, p := range enPhrases {
		if strings.Contains(s, p) {
			en++
		}
	}
	if en > 0 && id == 0 {
		return LangEN
	}
	return LangID
}

// timeRule maps a phrase to the instant it names. Order matters:
// "lusa kemarin" must be tried before "kemarin".
type timeRule struct {
	re   *regexp.Regexp
	when func(now time.Time, m []string) time.Time
}

var timeRules = []timeRule{
	{regexp.MustCompile(`\b(?:lusa kemarin|2 hari lalu|dua hari lalu|two days ago)\b`),
		func(now time.Time, _ []string) time.Time { return noonOn(now, -2) }},
	{regexp.MustCompile(`\b(?:tadi pagi|pagi ini|this morning)\b`),
		func(now time.Time, _ []string) time.Time { return atHour(now, 0, 8) }},
	{regexp.MustCompile(`\btadi siang\b`),
		func(now time.Time, _ []string) time.Time { return atHour(now, 0, 12) }},
	{regexp.MustCompile(`\b(?:tadi malam|semalam|last night)\b`),
		func(now time.Time, _ []string) time.Time { return atHour(now, -1, 20) }},
	{regexp.MustCompile(`\b(?:kemarin|yesterday)\b`),
		func(now time.Time, _ []string) time.Time { return noonOn(now, -1) }},
	{regexp.MustCompile(`\b(?:tgl|tanggal)\s+(\d{1,2})\b`), dayOfMonth},
	{regexp.MustCompile(`\bon the (\d{1,2})(?:st|nd|rd|th)\b`), dayOfMonth},
}

// extractTime pulls at most one time phrase out of the segment and
// returns the remainder plus the instant it names.
func extractTime(seg string, now time.Time) (string, time.Time) {
	for _, r := range timeRules {
		loc := r.re.FindStringSubmatchIndex(seg)
		if loc == nil {
			continue
		}
		m := make([]string, 0, len(loc)/2)
		for i := 0; i < len(loc); i += 2 {
			if loc[i] < 0 {
				m = append(m, "")
				continue
			}
			m = append(m, seg[loc[i]:loc[i+1]])
		}
		return cut(seg, []span{{loc[0], loc[1]}}), r.when(now, m)
	}
	return seg, now
}

// noonOn is the canonical timestamp for a day the user dated but did
// not time.
func noonOn(now time.Time, dayShift int) time.Time {
	d := now.AddDate(0, 0, dayShift)
	return time.Date(d.Year(), d.Month(), d.Day(), 12, 0, 0, 0, now.Location())
}

func atHour(now time.Time, dayShift, hour int) time.Time {
	d := now.AddDate(0, 0, dayShift)
	return time.Date(d.Year(), d.Month(), d.Day(), hour, 0, 0, 0, now.Location())
}

// dayOfMonth reads "tgl 5": this month when that day has passed,
// otherwise the same day last month — people log the past, not the
// future.
func dayOfMonth(now time.Time, m []string) time.Time {
	day, err := strconv.Atoi(m[1])
	if err != nil || day < 1 || day > 31 {
		return now
	}
	t := time.Date(now.Year(), now.Month(), day, 12, 0, 0, 0, now.Location())
	if t.After(now) {
		t = t.AddDate(0, -1, 0)
	}
	return t
}

type span struct{ start, end int }

// cut blanks out the given ranges, keeping a space so neighbouring
// words never fuse.
func cut(s string, spans []span) string {
	if len(spans) == 0 {
		return s
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	var b strings.Builder
	prev := 0
	for _, sp := range spans {
		if sp.start < prev {
			continue
		}
		b.WriteString(s[prev:sp.start])
		b.WriteByte(' ')
		prev = sp.end
	}
	b.WriteString(s[prev:])
	return normalize(b.String())
}

type moneyToken struct {
	value int64
	span  span
}

// extractAmount applies the gate from the spec: one token is the
// amount, `bayar X … kembali Y` is X−Y, and anything else with two or
// more tokens is a Hold. An incomplete pay/change pair ("kopi 25k bayar
// 50k") is exactly the ambiguity the gate exists for, so it holds too.
func extractAmount(seg string) (int64, *HoldReason, string) {
	var toks []moneyToken
	var spans []span
	for _, loc := range moneyRe.FindAllStringIndex(seg, -1) {
		v, ok := money.ParseToken(seg[loc[0]:loc[1]])
		if !ok {
			continue
		}
		toks = append(toks, moneyToken{value: v, span: span{loc[0], loc[1]}})
		spans = append(spans, span{loc[0], loc[1]})
	}

	// Role markers never belong to the merchant name.
	payLoc := payWordRe.FindStringIndex(seg)
	changeLoc := changeWordRe.FindStringIndex(seg)
	if payLoc != nil {
		spans = append(spans, span{payLoc[0], payLoc[1]})
	}
	if changeLoc != nil {
		spans = append(spans, span{changeLoc[0], changeLoc[1]})
	}
	rest := cut(seg, spans)

	switch {
	case len(toks) == 0:
		return 0, &HoldReason{Code: HoldNoAmount, Question: "q_amount"}, rest
	case len(toks) == 1:
		return toks[0].value, nil, rest
	}

	paid := firstTokenAfter(toks, payLoc)
	change := firstTokenAfter(toks, changeLoc)
	if paid != nil && change != nil {
		if net := paid.value - change.value; net > 0 {
			return net, nil, rest
		}
		return 0, &HoldReason{Code: HoldAmbiguousAmounts, Question: "q_amount_roles"}, rest
	}
	// A lone pay/change marker leaves the roles unassigned; only a
	// source marker ("dari bca") keeps the first token unambiguous.
	if payLoc == nil && changeLoc == nil && srcRoleRe.MatchString(seg) {
		return toks[0].value, nil, rest
	}
	return 0, &HoldReason{Code: HoldAmbiguousAmounts, Question: "q_amount_roles"}, rest
}

func firstTokenAfter(toks []moneyToken, loc []int) *moneyToken {
	if loc == nil {
		return nil
	}
	for i := range toks {
		if toks[i].span.start >= loc[1] {
			return &toks[i]
		}
	}
	return nil
}

// extractHints consumes every explicit clause: the destination of a
// transfer, then the pay clauses ("pakai bca"), the from clauses ("dari
// alfamart") and the place clauses ("di indomaret"), in that order. A
// bare trailing word ("bensin 20k cash") is deliberately left in the
// merchant: the harness tests it against the Source whitelist, the
// parser does not guess. Nor does it decide which "dari" is a Source —
// it does not know the Sources.
func (d *Draft) extractHints(rest string) string {
	if d.Type == TypeTransfer {
		rest, d.DestHint = takeHint(rest, destHintRe)
	}
	rest, d.Pays = takeAll(rest, payHintRe)
	rest, d.Froms = takeAll(rest, fromHintRe)
	rest, d.Places = takeAll(rest, placeHintRe)
	return rest
}

// takeAll consumes every clause the marker introduces, not just the
// first: "tissu dari alfamart dari bri" names two things and dropping
// either one leaves it inside the merchant.
func takeAll(rest string, re *regexp.Regexp) (string, []string) {
	var out []string
	for {
		loc := re.FindStringSubmatchIndex(rest)
		if loc == nil {
			return rest, out
		}
		if word := trimClause(rest[loc[2]:loc[3]]); word != "" {
			out = append(out, word)
		}
		rest = cut(rest, []span{{loc[0], loc[1]}})
	}
}

func takeHint(rest string, re *regexp.Regexp) (string, string) {
	loc := re.FindStringSubmatchIndex(rest)
	if loc == nil {
		return rest, ""
	}
	return cut(rest, []span{{loc[0], loc[1]}}), rest[loc[2]:loc[3]]
}

func merchantOf(rest string, typ string) string {
	words := strings.Fields(rest)
	for len(words) > 0 {
		w := words[0]
		drop := leadVerbs[w] ||
			(typ == TypeTransfer && leadTransfer[w]) ||
			(typ == TypeRefund && leadRefund[w])
		if !drop {
			break
		}
		words = words[1:]
	}
	return strings.Join(words, " ")
}
