package llm

import (
	"context"
	"encoding/base64"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// ReceiptItem is one purchased line the model read off the photo. The
// amount is in the printed currency's smallest unit; Go never rescales
// it (IDR's minor unit is the whole rupiah).
type ReceiptItem struct {
	Name               string
	Qty                float64
	AmountMinor        int64
	CategoryID         *uuid.UUID
	CategoryConfidence float64
}

// ReceiptRead is one photo's whole reading. Nothing here is trusted as
// money yet: the caller's deterministic gate decides whether the
// breakdown is consistent with the printed total before anything becomes
// a ledger line (ADR-0003).
type ReceiptRead struct {
	// ImageReceived is the model's own statement that an image was
	// attached and visible to it. A text-only model, or a router that
	// strips the image part, answers false — that is an outage, not a
	// non-receipt.
	ImageReceived bool
	// ServedModel is the model name the provider reports in its reply; a
	// router may alias the configured name to something else, and the
	// photo log needs the real one.
	ServedModel string

	DocumentType  string // receipt | payment_screenshot | statement | other
	Legible       bool
	Issues        []string // blur | glare | cropped | too_small | not_a_receipt
	Merchant      string
	Date          string // YYYY-MM-DD or ""
	Currency      string // ISO code or ""
	HasTotalLine  bool
	TotalMinor    int64 // 0 when unknown
	SubtotalMinor int64
	DiscountMinor int64  // positive number = reduction
	TaxMinor      int64  // tax + service charge combined
	RoundingMinor int64  // signed
	PaymentMethod string // free text as printed: "DEBIT BCA", "QRIS", "CASH", ""
	Direction     string // payment_screenshot only: out | in | ""
	Counterparty  string // payment_screenshot only: the other party's displayed name
	Items         []ReceiptItem

	PromptTokens     int
	CompletionTokens int
}

// receiptSystem is the whole contract with the vision model. The two
// laws it must obey: never compute a total (only transcribe the printed
// TOTAL line), and never guess a digit it cannot read.
const receiptSystem = "You read one photo of a store receipt, restaurant bill or payment-app screenshot for a personal finance app in Indonesia. " +
	"Return only JSON matching the schema. " +
	"All amounts are integers in the smallest unit of the printed currency: whole rupiah for IDR (never decimals), cents for currencies that print two decimals. " +
	"`total_minor` is the final amount actually paid after discount, tax, service and rounding, exactly as printed on the TOTAL line — never compute it. " +
	"If no TOTAL line is visible set `has_total_line=false` and `total_minor=0`. " +
	"If the text cannot be read reliably set `legible=false`, `total_minor=0` and list the problem in `issues` (blur, glare, cropped, too_small, not_a_receipt). " +
	"Never guess digits you cannot read. " +
	"`items` are the purchased lines only — not subtotal, tax, discount, rounding, cash or change. Ignore handwriting and stamps. " +
	"For each item choose `category_id` from the offered list only, or null; `category_confidence` 0..1. " +
	"For a payment-app screenshot (GoPay, OVO, DANA, BCA, QRIS, etc.) set `document_type=payment_screenshot`, one item named after the merchant/counterparty with the paid amount, " +
	"`direction=out` for money leaving, `in` for money received, `counterparty` = the displayed other party. " +
	"A bank statement, e-wallet history list, or BNPL/paylater bill with several transactions is `document_type=statement`. " +
	"Set `image_received=true` only when an image is attached to the message and you can see it; if there is no image or you cannot process images set `image_received=false`, `document_type=other`, `legible=false`, `total_minor=0` and `items=[]`. " +
	"Anything that is not money-related is `other`."

// isoDate is the only date shape accepted from the model; anything else
// is discarded rather than repaired.
var isoDate = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// ReadReceipt transcribes one photo. img is the raw image, inlined as a
// data URL; mime must already be the sniffed type. caption is the
// sender's own hint and cats the category whitelist — an id outside it is
// dropped like everywhere else (ADR-0003).
func (c *Client) ReadReceipt(ctx context.Context, img []byte, mime, caption string, cats []Candidate) (*ReceiptRead, error) {
	if !c.Enabled(ctx) {
		return nil, ErrUnavailable
	}

	var prompt strings.Builder
	prompt.WriteString("Caption from the sender: " + fallback(caption, "(none)") + ".\n")
	if len(cats) == 0 {
		prompt.WriteString("Categories you may use: none — always answer category_id null.\n")
	} else {
		prompt.WriteString("Categories you may use (id - name - hint):\n")
		writeCandidates(&prompt, cats)
	}

	schema := objectSchema(map[string]any{
		"image_received": map[string]any{"type": "boolean"},
		"document_type":  map[string]any{"type": "string", "enum": []string{"receipt", "payment_screenshot", "statement", "other"}},
		"legible":        map[string]any{"type": "boolean"},
		"issues":         map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": []string{"blur", "glare", "cropped", "too_small", "not_a_receipt"}}},
		"merchant":       map[string]any{"type": "string"},
		"date":           map[string]any{"type": "string"},
		"currency":       map[string]any{"type": "string"},
		"has_total_line": map[string]any{"type": "boolean"},
		"total_minor":    map[string]any{"type": "integer"},
		"subtotal_minor": map[string]any{"type": "integer"},
		"discount_minor": map[string]any{"type": "integer"},
		"tax_minor":      map[string]any{"type": "integer"},
		"rounding_minor": map[string]any{"type": "integer"},
		"payment_method": map[string]any{"type": "string"},
		"direction":      map[string]any{"type": "string", "enum": []string{"out", "in", ""}},
		"counterparty":   map[string]any{"type": "string"},
		"items": map[string]any{
			"type": "array",
			"items": objectSchema(map[string]any{
				"name":                map[string]any{"type": "string"},
				"qty":                 map[string]any{"type": "number"},
				"amount_minor":        map[string]any{"type": "integer"},
				"category_id":         map[string]any{"type": []string{"string", "null"}},
				"category_confidence": map[string]any{"type": "number"},
			}, "name", "qty", "amount_minor", "category_id", "category_confidence"),
		},
	},
		"image_received",
		"document_type", "legible", "issues", "merchant", "date", "currency",
		"has_total_line", "total_minor", "subtotal_minor", "discount_minor",
		"tax_minor", "rounding_minor", "payment_method", "direction",
		"counterparty", "items",
	)

	var out struct {
		ImageReceived bool     `json:"image_received"`
		DocumentType  string   `json:"document_type"`
		Legible       bool     `json:"legible"`
		Issues        []string `json:"issues"`
		Merchant      string   `json:"merchant"`
		Date          string   `json:"date"`
		Currency      string   `json:"currency"`
		HasTotalLine  bool     `json:"has_total_line"`
		TotalMinor    int64    `json:"total_minor"`
		SubtotalMinor int64    `json:"subtotal_minor"`
		DiscountMinor int64    `json:"discount_minor"`
		TaxMinor      int64    `json:"tax_minor"`
		RoundingMinor int64    `json:"rounding_minor"`
		PaymentMethod string   `json:"payment_method"`
		Direction     string   `json:"direction"`
		Counterparty  string   `json:"counterparty"`
		Items         []struct {
			Name               string  `json:"name"`
			Qty                float64 `json:"qty"`
			AmountMinor        int64   `json:"amount_minor"`
			CategoryID         *string `json:"category_id"`
			CategoryConfidence float64 `json:"category_confidence"`
		} `json:"items"`
	}

	messages := []chatMessage{
		{Role: "system", Content: receiptSystem},
		{Role: "user", Content: []contentPart{
			{Type: "text", Text: prompt.String()},
			{Type: "image_url", ImageURL: &imageURL{URL: dataURL(mime, img)}},
		}},
	}
	used, err := c.completeWith(ctx, true, messages, "receipt_read", schema, &out)
	if err != nil {
		return nil, err
	}

	read := &ReceiptRead{
		ImageReceived:    out.ImageReceived,
		DocumentType:     normalizeDocumentType(out.DocumentType),
		Legible:          out.Legible,
		Issues:           normalizeIssues(out.Issues),
		Merchant:         strings.TrimSpace(out.Merchant),
		Currency:         strings.ToUpper(strings.TrimSpace(out.Currency)),
		HasTotalLine:     out.HasTotalLine,
		TotalMinor:       nonNegative(out.TotalMinor),
		SubtotalMinor:    nonNegative(out.SubtotalMinor),
		DiscountMinor:    nonNegative(out.DiscountMinor),
		TaxMinor:         nonNegative(out.TaxMinor),
		RoundingMinor:    out.RoundingMinor,
		PaymentMethod:    strings.TrimSpace(out.PaymentMethod),
		Direction:        normalizeDirection(out.Direction),
		Counterparty:     strings.TrimSpace(out.Counterparty),
		PromptTokens:     used.PromptTokens,
		CompletionTokens: used.CompletionTokens,
		ServedModel:      used.Model,
	}
	if d := strings.TrimSpace(out.Date); isoDate.MatchString(d) {
		read.Date = d
	}
	for _, it := range out.Items {
		item := ReceiptItem{
			Name:        strings.TrimSpace(it.Name),
			Qty:         it.Qty,
			AmountMinor: nonNegative(it.AmountMinor),
		}
		if item.Qty < 0 {
			item.Qty = 0
		}
		if id, ok := match(it.CategoryID, cats); ok {
			item.CategoryID = id
			item.CategoryConfidence = clamp01(it.CategoryConfidence)
		}
		read.Items = append(read.Items, item)
	}
	return read, nil
}

// dataURL inlines the image bytes; the providers that read receipts all
// accept a base64 data URL and none of them can reach our gateway.
func dataURL(mime string, img []byte) string {
	if mime == "" {
		mime = "image/jpeg"
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(img)
}

// normalizeDocumentType enforces the enum: an unlisted answer is "other",
// which the gate refuses.
func normalizeDocumentType(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "receipt":
		return "receipt"
	case "payment_screenshot":
		return "payment_screenshot"
	case "statement":
		return "statement"
	default:
		return "other"
	}
}

func normalizeDirection(dir string) string {
	switch strings.ToLower(strings.TrimSpace(dir)) {
	case "out":
		return "out"
	case "in":
		return "in"
	default:
		return ""
	}
}

// normalizeIssues keeps only the known problem names, so the copy that
// explains the refusal never quotes a model invention.
func normalizeIssues(issues []string) []string {
	var out []string
	for _, raw := range issues {
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "blur":
			out = append(out, "blur")
		case "glare":
			out = append(out, "glare")
		case "cropped":
			out = append(out, "cropped")
		case "too_small":
			out = append(out, "too_small")
		case "not_a_receipt":
			out = append(out, "not_a_receipt")
		}
	}
	return out
}

func nonNegative(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

func clamp01(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 1:
		return 1
	default:
		return v
	}
}
