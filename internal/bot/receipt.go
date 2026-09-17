package bot

import (
	"fmt"
	"math/bits"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/llm"
	"github.com/glennprays/catet-backend/internal/money"
	"github.com/glennprays/catet-backend/internal/service"
	"github.com/google/uuid"
)

// receiptItem is one parked ledger line read off a photo. N is 1-based
// and becomes the line's external_id suffix, so a retried execution
// writes the same line instead of a second copy.
type receiptItem struct {
	N                  int        `json:"n"`
	Name               string     `json:"name"`
	Qty                float64    `json:"qty,omitempty"`
	AmountMinor        int64      `json:"amount_minor"`
	CategoryID         *uuid.UUID `json:"category_id,omitempty"`
	CategoryName       string     `json:"category_name,omitempty"`
	CategoryConfidence *float64   `json:"category_confidence,omitempty"`
	// SourceID overrides the draft's Source for this one line; it is set
	// only by a split payment. Part is "a"/"b" when one printed line had
	// to be cut across two payments, and extends the external_id so both
	// halves stay idempotent ("m1#3a").
	SourceID   *uuid.UUID `json:"source_id,omitempty"`
	SourceName string     `json:"source_name,omitempty"`
	Part       string     `json:"part,omitempty"`
}

// receiptPayment is one share of a split bill: who paid and how much.
// The shares always sum to the printed total.
type receiptPayment struct {
	SourceID    uuid.UUID `json:"source_id"`
	SourceName  string    `json:"source_name"`
	AmountMinor int64     `json:"amount_minor"`
}

// receiptParams is the Hold Queue payload of a photographed receipt: the
// whole draft, N lines and all, as both the chat executor and the
// dashboard read it. Items always sum to TotalMinor — the printed total
// is the only money fact a photo contributes (ADR-0003).
type receiptParams struct {
	Type                string        `json:"type"` // expense | income | transfer
	DocumentType        string        `json:"document_type"`
	Merchant            string        `json:"merchant"`
	TotalMinor          int64         `json:"total_minor"`
	Items               []receiptItem `json:"items"`
	SourceID            *uuid.UUID    `json:"source_id,omitempty"`
	SourceName          string        `json:"source_name,omitempty"`
	DestinationSourceID *uuid.UUID    `json:"destination_source_id,omitempty"`
	OccurredAt          time.Time     `json:"occurred_at"`
	ReceiptDate         string        `json:"receipt_date,omitempty"` // as printed, YYYY-MM-DD
	PaymentMethod       string        `json:"payment_method,omitempty"`
	MessageID           string        `json:"message_id"`
	OriginalText        string        `json:"original_text"` // the caption, may be ""
	ContentHash         string        `json:"content_hash"`
	DuplicateOf         *uuid.UUID    `json:"duplicate_of,omitempty"`
	// Inconsistent marks a draft whose printed breakdown missed the
	// printed total by more than `receiptTolerance`: the lines are
	// proportional shares of the total, never the printed item prices.
	Inconsistent bool     `json:"inconsistent"`
	Missing      []string `json:"missing"`
	// Payments is the split of one bill across several Sources. Empty
	// means the ordinary case: SourceID paid all of it.
	Payments []receiptPayment `json:"payments,omitempty"`
	// SourceBasis records how SourceID was decided (see source.go); the
	// question's wording and the dashboard's label both read it.
	SourceBasis string `json:"source_basis,omitempty"`
	// CategorySource is set to "user" once the user labels the whole
	// receipt in chat, so the write records a human decision, not a guess.
	CategorySource string `json:"category_source,omitempty"`
	// Chat and From address the inbound message the draft came from, so a
	// dashboard confirmation can still react on the original photo.
	Chat string `json:"chat,omitempty"`
	From string `json:"from,omitempty"`
}

// receiptInput is everything buildReceiptDraft needs besides the read
// itself. Keeping it a value makes the gate pure and table-testable.
type receiptInput struct {
	MessageID string
	Caption   string
	Now       time.Time
	Cats      []entity.Category
}

// receiptReject is a refusal: the template that answers and its data.
// Refusing is the normal outcome for a bad photo — nothing is parked.
type receiptReject struct {
	Template string
	Data     map[string]any
}

// taxLineName labels the one synthetic line a receipt may gain. It is a
// ledger note, not copy: it is stored on the transaction.
const taxLineName = "Pajak & biaya layanan"

// receiptTolerance is how far the printed breakdown may miss the printed
// total before the draft is flagged inconsistent: one thousand rupiah
// or one percent, whichever is larger. Below that the gap is rounding and
// unreadable small print; above it the item list means something else.
func receiptTolerance(total int64) int64 {
	if pct := total / 100; pct > 1000 {
		return pct
	}
	return 1000
}

// buildReceiptDraft turns one model reading into a parked draft, or
// refuses it. Every rule is a deterministic check on what the model
// reported; the model never decides whether its own reading is usable.
func buildReceiptDraft(r *llm.ReceiptRead, in receiptInput) (*receiptParams, *receiptReject) {
	if r == nil {
		return nil, &receiptReject{Template: "photo_not_receipt"}
	}

	// A model that never saw the photo says so and then has nothing to
	// report. That is our outage — a text-only model or a router that
	// dropped the image — never the user's photo, so it must not be
	// answered as "not a receipt". The guard is conjunctive on purpose:
	// a model that read a merchant or a total is believed even if it
	// forgot the flag.
	if !r.ImageReceived && r.DocumentType == "other" && !r.Legible && r.TotalMinor == 0 && len(r.Items) == 0 {
		return nil, &receiptReject{Template: "photo_vision_down"}
	}

	switch {
	case r.DocumentType == "statement":
		return nil, &receiptReject{Template: "photo_statement"}
	case r.DocumentType == "other", hasIssue(r.Issues, "not_a_receipt"):
		return nil, &receiptReject{Template: "photo_not_receipt"}
	}
	if !r.Legible {
		return nil, &receiptReject{Template: "photo_unreadable", Data: map[string]any{
			"Issue": firstIssue(r.Issues),
		}}
	}
	// The ledger is rupiah-only; a foreign receipt would need an FX rate
	// nobody entered (spec §4).
	if r.Currency != "" && r.Currency != "IDR" {
		return nil, &receiptReject{Template: "photo_currency", Data: map[string]any{
			"Currency": r.Currency,
		}}
	}
	// No printed TOTAL means no money fact at all — the top half of a long
	// receipt, most often.
	if !r.HasTotalLine || r.TotalMinor <= 0 {
		return nil, &receiptReject{Template: "photo_no_total"}
	}

	p := &receiptParams{
		Type:          "expense",
		DocumentType:  r.DocumentType,
		Merchant:      strings.TrimSpace(r.Merchant),
		TotalMinor:    r.TotalMinor,
		PaymentMethod: r.PaymentMethod,
		MessageID:     in.MessageID,
		OriginalText:  in.Caption,
	}

	// A printed date is kept only when it is a date this ledger accepts:
	// never in the future, never older than the memory window. Otherwise
	// the capture is dated when it arrived, and the photo's own date is
	// dropped rather than stored as a half-truth.
	p.OccurredAt = in.Now.UTC()
	if r.Date != "" {
		if noon, err := service.JakartaNoon(r.Date); err == nil {
			day := in.Now.Format("2006-01-02")
			today, dayErr := service.JakartaNoon(day)
			if dayErr == nil &&
				!noon.After(today) &&
				!noon.Before(today.AddDate(0, 0, -llm.MaxBackdateDays)) {
				p.ReceiptDate = r.Date
				p.OccurredAt = noon
			}
		}
	}

	if r.DocumentType == "payment_screenshot" {
		name := firstNonEmpty(r.Counterparty, r.Merchant, "Pembayaran")
		p.Merchant = firstNonEmpty(r.Counterparty, r.Merchant, "")
		p.Items = []receiptItem{{N: 1, Name: name, AmountMinor: r.TotalMinor}}
		p.Items[0].CategoryID, p.Items[0].CategoryConfidence = majorityCategory(r.Items)
		p.Items[0].CategoryName = categoryName(derefID(p.Items[0].CategoryID), in.Cats)
	} else {
		p.Items, p.Inconsistent = receiptLines(r, in.Cats)
	}
	if p.Merchant == "" {
		p.Merchant = "Struk"
	}
	for i := range p.Items {
		if p.Items[i].Name == "" {
			p.Items[i].Name = p.Merchant
		}
	}
	return p, nil
}

// receiptLines splits the printed total into ledger lines. It trusts the
// item list only as *weights*: the amounts themselves are always shares
// of the printed total, so the lines can never sum to something the
// receipt does not say. The second return reports that the printed
// breakdown did not reconcile with the printed total; the lines are kept
// anyway — proportional shares of the total — because every receipt is a
// Hold, so the split is confirmed explicitly and never invented silently.
func receiptLines(r *llm.ReceiptRead, cats []entity.Category) ([]receiptItem, bool) {
	total := r.TotalMinor

	var sum int64
	usable := len(r.Items) > 0
	for _, it := range r.Items {
		if it.AmountMinor <= 0 {
			usable = false
			break
		}
		sum += it.AmountMinor
	}
	// Without usable weights there is nothing to split by: one line at
	// the printed total. Items that were read but unpriced still mean the
	// breakdown failed; a receipt with no items at all is simply a total.
	if !usable {
		id, conf := majorityCategory(r.Items)
		name := strconv.Itoa(len(r.Items)) + " item"
		if len(r.Items) == 0 {
			name = "Total"
		}
		return []receiptItem{{
			N: 1, Name: strings.TrimSpace(r.Merchant + " · " + name),
			AmountMinor: total, CategoryID: id, CategoryConfidence: conf,
			CategoryName: categoryName(derefID(id), cats),
		}}, len(r.Items) > 0
	}

	// The consistency check: does the printed breakdown explain the
	// printed total? A miss larger than the tolerance means we misread
	// the lines, the discount or the tax (ADR-0003). The item list is
	// still the best weighting we have, so the lines survive as
	// proportional shares — flagged, for the user to confirm or collapse.
	expected := sum - r.DiscountMinor + r.TaxMinor + r.RoundingMinor
	inconsistent := abs64(expected-total) > receiptTolerance(total)

	// Tax and service charge are one visible line when they add to the
	// bill; a negative rounding adjustment is absorbed by the shares
	// instead of becoming a line of its own. An inconsistent read gets no
	// synthetic line at all: those very figures are what failed to
	// reconcile, so they are not trusted as money.
	extra := r.TaxMinor + r.RoundingMinor
	itemsTarget := total
	if !inconsistent && extra > 0 && extra < total {
		itemsTarget = total - extra
	} else {
		extra = 0
	}

	weights := make([]int64, 0, len(r.Items))
	for _, it := range r.Items {
		weights = append(weights, it.AmountMinor)
	}
	shares := allocate(itemsTarget, weights)

	lines := make([]receiptItem, 0, len(r.Items)+1)
	for i, it := range r.Items {
		line := receiptItem{
			N: len(lines) + 1, Name: strings.TrimSpace(it.Name), Qty: it.Qty,
			AmountMinor: shares[i], CategoryID: it.CategoryID,
			CategoryName: categoryName(derefID(it.CategoryID), cats),
		}
		if it.CategoryID != nil {
			conf := it.CategoryConfidence
			line.CategoryConfidence = &conf
		}
		lines = append(lines, line)
	}
	if extra > 0 {
		id, conf := majorityCategory(r.Items)
		lines = append(lines, receiptItem{
			N: len(lines) + 1, Name: taxLineName, AmountMinor: extra,
			CategoryID: id, CategoryConfidence: conf,
			CategoryName: categoryName(derefID(id), cats),
		})
	}
	return lines, inconsistent
}

// allocate splits total across weights with largest-remainder rounding,
// so the shares sum to exactly total. Products are computed in 128 bits:
// total*weight overflows int64 for absurd-but-representable figures, and
// money must never wrap.
func allocate(total int64, weights []int64) []int64 {
	out := make([]int64, len(weights))
	var sumW int64
	for _, w := range weights {
		sumW += w
	}
	if len(weights) == 0 || sumW <= 0 || total <= 0 {
		return out
	}

	rems := make([]int64, len(weights))
	assigned := int64(0)
	for i, w := range weights {
		hi, lo := bits.Mul64(uint64(total), uint64(w))
		quo, rem := bits.Div64(hi, lo, uint64(sumW))
		out[i] = int64(quo)
		rems[i] = int64(rem)
		assigned += out[i]
	}
	// Hand the rounding leftovers to the largest remainders, earliest
	// line first on a tie.
	for left := total - assigned; left > 0; left-- {
		best, bestRem := -1, int64(-1)
		for i := range rems {
			if rems[i] > bestRem {
				best, bestRem = i, rems[i]
			}
		}
		if best < 0 {
			break
		}
		out[best]++
		rems[best] = -1
	}
	return out
}

// categoryTally holds the majority rule in one place: the most frequent
// Category among a receipt's lines wins, an even disagreement wins
// nothing, and the winner keeps the highest confidence seen for it.
type categoryTally struct {
	counts map[uuid.UUID]int
	confs  map[uuid.UUID]float64
}

func newCategoryTally(n int) categoryTally {
	return categoryTally{
		counts: make(map[uuid.UUID]int, n),
		confs:  make(map[uuid.UUID]float64, n),
	}
}

func (t categoryTally) add(id *uuid.UUID, conf float64) {
	if id == nil {
		return
	}
	t.counts[*id]++
	if conf > t.confs[*id] {
		t.confs[*id] = conf
	}
}

func (t categoryTally) winner() (*uuid.UUID, *float64) {
	var winner uuid.UUID
	best, tied := 0, false
	for id, n := range t.counts {
		switch {
		case n > best:
			winner, best, tied = id, n, false
		case n == best:
			tied = true
		}
	}
	if best == 0 || tied {
		return nil, nil
	}
	id, conf := winner, t.confs[winner]
	return &id, &conf
}

// majorityCategory is the label a synthetic line inherits: the most
// frequent category among the read items, or none when the items
// disagree evenly.
func majorityCategory(items []llm.ReceiptItem) (*uuid.UUID, *float64) {
	tally := newCategoryTally(len(items))
	for _, it := range items {
		tally.add(it.CategoryID, it.CategoryConfidence)
	}
	return tally.winner()
}

// majorityLineCategory is majorityCategory over parked lines: the label
// a collapsed receipt inherits from the split it replaces.
func majorityLineCategory(items []receiptItem) (*uuid.UUID, *float64) {
	tally := newCategoryTally(len(items))
	for _, it := range items {
		conf := 0.0
		if it.CategoryConfidence != nil {
			conf = *it.CategoryConfidence
		}
		tally.add(it.CategoryID, conf)
	}
	return tally.winner()
}

// receiptSummaryData is the Hold Queue row label's slots. The copy that
// assembles them lives in the templates, so the label is in the same
// language as the reply that shows it.
func receiptSummaryData(p *receiptParams, lang string) map[string]any {
	date := formatDay(p.OccurredAt.In(service.JakartaNow().Location()), lang)
	if p.ReceiptDate != "" {
		if noon, err := service.JakartaNoon(p.ReceiptDate); err == nil {
			date = formatDay(noon, lang)
		}
	}
	return map[string]any{
		"Payment":      p.DocumentType == "payment_screenshot",
		"Merchant":     p.Merchant,
		"Date":         date,
		"N":            len(p.Items),
		"Total":        money.FormatIDR(p.TotalMinor),
		"Duplicate":    p.DuplicateOf != nil,
		"Inconsistent": p.Inconsistent,
	}
}

// hasIssue reports whether the model flagged a specific problem.
func hasIssue(issues []string, want string) bool {
	for _, i := range issues {
		if i == want {
			return true
		}
	}
	return false
}

// firstIssue names the problem the refusal copy explains. "" renders as
// the generic "unreadable" wording.
func firstIssue(issues []string) string {
	if len(issues) == 0 {
		return ""
	}
	return issues[0]
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// firstOf is firstIssue for any clause list: the first entry or "".
func firstOf(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func derefID(id *uuid.UUID) uuid.UUID {
	if id == nil {
		return uuid.Nil
	}
	return *id
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// receiptExternalID is the per-line idempotency key: the same photo
// executed twice writes the same N lines, never 2N.
func receiptExternalID(messageID string, n int) string {
	return fmt.Sprintf("%s#%d", messageID, n)
}

// applySplit records that one bill was paid from several Sources
// ("20rb cash sisanya bca"). It is pure: every refusal is a template to
// render, and the draft is left untouched unless the whole split adds
// up. The money law still holds — the shares are checked against the
// printed total and never against each other's wishful thinking.
func applySplit(p *receiptParams, payments []llm.Payment, sources []entity.Source) *receiptReject {
	names := map[string]any{"Sources": sourceNames(sources)}

	// Resolve and merge: "10rb bca, 10rb bca" is one payment of 20rb.
	resolved := make([]receiptPayment, 0, len(payments))
	index := map[uuid.UUID]int{}
	blanks := 0
	var stated int64
	for _, pay := range payments {
		if pay.SourceID == nil {
			return &receiptReject{Template: "receipt_split_unknown_source", Data: names}
		}
		src := findSource(*pay.SourceID, sources)
		if src == nil {
			return &receiptReject{Template: "receipt_split_unknown_source", Data: names}
		}
		var amount int64
		if pay.AmountText == "" {
			// "sisanya": exactly one share may be the remainder, because
			// two unknowns have no single solution.
			blanks++
			if blanks > 1 {
				return &receiptReject{Template: "receipt_split_mismatch", Data: map[string]any{
					"Parts": splitParts(resolved), "Total": money.FormatIDR(p.TotalMinor),
				}}
			}
		} else {
			parsed, ok := money.ParseToken(pay.AmountText)
			if !ok || parsed <= 0 {
				return &receiptReject{Template: "receipt_split_mismatch", Data: map[string]any{
					"Parts": splitParts(resolved), "Total": money.FormatIDR(p.TotalMinor),
				}}
			}
			amount = parsed
			stated += parsed
		}
		if at, ok := index[src.ID]; ok {
			if amount == 0 {
				// The remainder cannot also be a named share.
				return &receiptReject{Template: "receipt_split_mismatch", Data: map[string]any{
					"Parts": splitParts(resolved), "Total": money.FormatIDR(p.TotalMinor),
				}}
			}
			resolved[at].AmountMinor += amount
			continue
		}
		index[src.ID] = len(resolved)
		resolved = append(resolved, receiptPayment{
			SourceID: src.ID, SourceName: src.Name, AmountMinor: amount,
		})
	}
	if len(resolved) < 2 {
		return &receiptReject{Template: "receipt_split_mismatch", Data: map[string]any{
			"Parts": splitParts(resolved), "Total": money.FormatIDR(p.TotalMinor),
		}}
	}

	if blanks == 1 {
		remainder := p.TotalMinor - stated
		if remainder <= 0 {
			return &receiptReject{Template: "receipt_split_mismatch", Data: map[string]any{
				"Parts": splitParts(resolved), "Total": money.FormatIDR(p.TotalMinor),
			}}
		}
		for i := range resolved {
			if resolved[i].AmountMinor == 0 {
				resolved[i].AmountMinor = remainder
				break
			}
		}
	} else if stated != p.TotalMinor {
		return &receiptReject{Template: "receipt_split_mismatch", Data: map[string]any{
			"Parts": splitParts(resolved), "Total": money.FormatIDR(p.TotalMinor),
		}}
	}

	p.Payments = resolved
	p.Items = splitLines(p.Items, resolved)
	p.SourceID = &resolved[0].SourceID
	p.SourceName = resolved[0].SourceName
	p.SourceBasis = basisUser
	p.Missing = nil
	return nil
}

// splitParts renders the shares the user named, for the refusal that
// tells them what did not add up.
func splitParts(payments []receiptPayment) string {
	parts := make([]string, 0, len(payments))
	for _, pay := range payments {
		parts = append(parts, money.FormatIDR(pay.AmountMinor))
	}
	return strings.Join(parts, " + ")
}

// splitLines assigns every ledger line to the Source that paid for it.
// Whole lines are kept together wherever possible — the biggest first,
// so the fewest lines get cut — and only the line that straddles two
// payments is divided, into an "a" and a "b" half. By construction the
// lines still sum to the printed total.
func splitLines(items []receiptItem, payments []receiptPayment) []receiptItem {
	if len(items) == 0 || len(payments) == 0 {
		return items
	}
	order := make([]receiptItem, len(items))
	copy(order, items)
	sort.SliceStable(order, func(i, j int) bool {
		return order[i].AmountMinor > order[j].AmountMinor
	})

	out := make([]receiptItem, 0, len(items)+len(payments)-1)
	pending := order
	for pi := range payments {
		remaining := payments[pi].AmountMinor
		last := pi == len(payments)-1
		rest := make([]receiptItem, 0, len(pending))
		for _, item := range pending {
			switch {
			case last || item.AmountMinor <= remaining:
				line := item
				line.SourceID = &payments[pi].SourceID
				line.SourceName = payments[pi].SourceName
				out = append(out, line)
				remaining -= item.AmountMinor
			case remaining > 0:
				// This line straddles the boundary: the head belongs to
				// this payment, the tail carries into the next one.
				head := item
				head.AmountMinor = remaining
				head.Part = "a"
				head.SourceID = &payments[pi].SourceID
				head.SourceName = payments[pi].SourceName
				out = append(out, head)

				tail := item
				tail.AmountMinor = item.AmountMinor - remaining
				tail.Part = "b"
				rest = append(rest, tail)
				remaining = 0
			default:
				rest = append(rest, item)
			}
		}
		pending = rest
	}

	// Back to the order the receipt printed, with a cut line's halves
	// adjacent and "a" first.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].N != out[j].N {
			return out[i].N < out[j].N
		}
		return out[i].Part < out[j].Part
	})
	return out
}
