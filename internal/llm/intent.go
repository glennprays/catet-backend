package llm

import (
	"context"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// Intent kinds. The router classifies one message into exactly one of
// these; anything it cannot map is KindUnknown, never a guess.
const (
	KindCapture = "capture"
	KindCorrect = "correct"
	KindUndo    = "undo"
	KindSummary = "summary"
	KindBalance = "balance"
	KindAnswer  = "answer"
	KindUnknown = "unknown"
)

// MaxBackdateDays bounds a date the model may claim — a relative offset
// in the intent router, a printed date on a receipt. Anything older is a
// hallucination, not a memory: it falls back to today.
const MaxBackdateDays = 31

// OpenQuestion describes the one outstanding question of the chat, so
// the router can tell "bca" (an answer) from "bca saldo berapa" (a
// balance question).
type OpenQuestion struct {
	Expect  string // source | destination | amount | yes_no | refund_target
	Summary string
}

// RecentItem is one line of the last chat batch, already rendered. N is
// 1-based and is what a correction points at.
type RecentItem struct {
	N        int
	Type     string
	Merchant string
	Amount   string
	Category string
	Source   string
}

// IntentRequest is everything the router may see. The two candidate
// lists are the whitelist: an id outside them is dropped on parse.
type IntentRequest struct {
	Lang       string
	Text       string
	Today      string // YYYY-MM-DD in Asia/Jakarta
	Open       *OpenQuestion
	Recent     []RecentItem
	Sources    []Candidate
	Categories []Candidate
}

// IntentDraft is one transaction the router read out of the message.
// AmountText is copied verbatim from the message and is verified by the
// caller against the deterministic money grammar — the model never
// contributes a number (ADR-0003).
type IntentDraft struct {
	Type                string // expense | income | transfer
	AmountText          string
	Merchant            string
	SourceID            *uuid.UUID
	DestinationSourceID *uuid.UUID
	CategoryID          *uuid.UUID
	DayOffset           int // 0 today, -1 yesterday, … (never positive)
}

// Correction is an edit of one recent transaction. Every field is
// optional; nil means "leave as is".
type Correction struct {
	TargetIndex int // 1-based into Recent; 0 = most recent
	CategoryID  *uuid.UUID
	SourceID    *uuid.UUID
	Merchant    *string
	AmountText  *string
	DayOffset   *int
}

// AnswerFields is everything one message may settle about the open
// draft. A chat answer is rarely a bare "ya": "bayar pake BCA, ya catat"
// confirms, names the Source, and leaves nothing to ask. Nil pointers
// mean "not mentioned"; an empty Confirm means the message edited the
// draft without accepting or rejecting it.
type AnswerFields struct {
	Confirm    string // "yes" | "no" | ""
	SourceID   *uuid.UUID
	Payments   []Payment // split payment: >1 entry
	CategoryID *uuid.UUID
	Merchant   *string
	DayOffset  *int
}

// Payment is one share of a split bill. AmountText is copied verbatim
// from the message and is turned into money by the caller; an empty
// AmountText is "sisanya" — the remainder of the printed total, which
// only the caller can compute.
type Payment struct {
	SourceID   *uuid.UUID
	AmountText string
}

// answerPayload is the wire shape of AnswerFields: ids arrive as raw
// strings and are whitelisted on the way in, so the model can never
// name a Source or category this ledger does not have.
type answerPayload struct {
	Confirm  string  `json:"confirm"`
	SourceID *string `json:"source_id"`
	Payments []struct {
		SourceID   *string `json:"source_id"`
		AmountText string  `json:"amount_text"`
	} `json:"payments"`
	CategoryID *string `json:"category_id"`
	Merchant   *string `json:"merchant"`
	DayOffset  *int    `json:"day_offset"`
}

// decodeAnswer whitelists everything the router claims about the open
// draft. A payload that settles nothing at all is no answer: it returns
// nil, and the harness keeps the question standing.
func decodeAnswer(a *answerPayload, req IntentRequest) *AnswerFields {
	if a == nil {
		return nil
	}
	out := &AnswerFields{Confirm: normalizeConfirm(a.Confirm)}
	if id, ok := match(a.SourceID, req.Sources); ok {
		out.SourceID = id
	}
	if id, ok := match(a.CategoryID, req.Categories); ok {
		out.CategoryID = id
	}
	for _, p := range a.Payments {
		payment := Payment{AmountText: strings.TrimSpace(p.AmountText)}
		if id, ok := match(p.SourceID, req.Sources); ok {
			payment.SourceID = id
		}
		out.Payments = append(out.Payments, payment)
	}
	if a.Merchant != nil {
		if m := strings.TrimSpace(*a.Merchant); m != "" {
			out.Merchant = &m
		}
	}
	if a.DayOffset != nil {
		off := clampOffset(*a.DayOffset)
		out.DayOffset = &off
	}
	if out.Confirm == "" && out.SourceID == nil && out.CategoryID == nil &&
		out.Merchant == nil && out.DayOffset == nil && len(out.Payments) == 0 {
		return nil
	}
	return out
}

// normalizeConfirm enforces the yes/no enum; anything else is "the
// message did not accept or reject the draft".
func normalizeConfirm(confirm string) string {
	switch strings.ToLower(strings.TrimSpace(confirm)) {
	case "yes":
		return "yes"
	case "no":
		return "no"
	default:
		return ""
	}
}

// Intent is the router's answer: one kind plus that kind's payload.
type Intent struct {
	Kind          string
	Drafts        []IntentDraft
	Correction    *Correction
	UndoIndex     int    // 0 = the whole batch
	Period        string // today | week
	BalanceSource *uuid.UUID
	Answer        *AnswerFields
}

const intentSystem = "You are the intent router of a personal-finance WhatsApp bot for one Indonesian user. " +
	"Classify the message into exactly one intent and fill only that intent's fields. " +
	"Rules: copy every amount VERBATIM from the message into amount_text (never compute or reformat); " +
	"choose ids only from the lists given, otherwise null; " +
	"\"correct\" means the user is fixing one of the RECENT transactions (words like itu, tadi, ganti, salah, bukan, seharusnya); " +
	"\"answer\" is only valid when an OPEN QUESTION is shown; fill every field the message settles about that draft: " +
	"confirm yes/no, the paying source, per-source amounts for a split payment (amount_text VERBATIM, empty for \"sisanya\"/\"the rest\"), " +
	"one category for all its lines, a corrected merchant name, a day offset; " +
	"edits to the draft under an OPEN QUESTION are \"answer\", never \"correct\"; " +
	"\"balance\" asks how much money is in a source or overall; " +
	"\"summary\" asks about spending today or this week; " +
	"anything else, including greetings and questions you cannot map, is \"unknown\"."

// Intent classifies one inbound message. A nil client is the documented
// "LLM disabled" state: the harness then falls back to rules only.
func (c *Client) Intent(ctx context.Context, req IntentRequest) (*Intent, error) {
	if c == nil {
		return nil, ErrUnavailable
	}

	var user strings.Builder
	user.WriteString("Today: " + req.Today + "\n")
	user.WriteString("Language: " + fallback(req.Lang, "id") + "\n")
	if req.Open != nil {
		user.WriteString("OPEN QUESTION: expect=" + req.Open.Expect + " about \"" + req.Open.Summary + "\"\n")
	}
	if len(req.Recent) == 0 {
		user.WriteString("RECENT: none\n")
	} else {
		user.WriteString("RECENT (most recent last):\n")
		for _, it := range req.Recent {
			user.WriteString(strconv.Itoa(it.N) + ". " + it.Type)
			for _, field := range []string{it.Amount, it.Merchant, it.Category, it.Source} {
				if field != "" {
					user.WriteString(" · " + field)
				}
			}
			user.WriteString("\n")
		}
	}
	user.WriteString("SOURCES:\n")
	writeCandidates(&user, req.Sources)
	user.WriteString("CATEGORIES:\n")
	writeCandidates(&user, req.Categories)
	user.WriteString("MESSAGE: " + req.Text)

	schema := objectSchema(map[string]any{
		"intent": map[string]any{
			"type": "string",
			"enum": []string{KindCapture, KindCorrect, KindUndo, KindSummary, KindBalance, KindAnswer, KindUnknown},
		},
		"drafts": map[string]any{
			"type": "array",
			"items": objectSchema(map[string]any{
				"type":                  map[string]any{"type": "string", "enum": []string{"expense", "income", "transfer"}},
				"amount_text":           map[string]any{"type": "string"},
				"merchant":              map[string]any{"type": "string"},
				"source_id":             map[string]any{"type": []string{"string", "null"}},
				"destination_source_id": map[string]any{"type": []string{"string", "null"}},
				"category_id":           map[string]any{"type": []string{"string", "null"}},
				"day_offset":            map[string]any{"type": "integer"},
			}, "type", "amount_text", "merchant", "source_id", "destination_source_id", "category_id", "day_offset"),
		},
		"correction": map[string]any{
			"type": []string{"object", "null"},
			"properties": map[string]any{
				"target_index": map[string]any{"type": "integer"},
				"category_id":  map[string]any{"type": []string{"string", "null"}},
				"source_id":    map[string]any{"type": []string{"string", "null"}},
				"merchant":     map[string]any{"type": []string{"string", "null"}},
				"amount_text":  map[string]any{"type": []string{"string", "null"}},
				"day_offset":   map[string]any{"type": []string{"integer", "null"}},
			},
		},
		"undo_index":        map[string]any{"type": "integer"},
		"period":            map[string]any{"type": "string", "enum": []string{"today", "week", ""}},
		"balance_source_id": map[string]any{"type": []string{"string", "null"}},
		"answer": map[string]any{
			"type": []string{"object", "null"},
			"properties": map[string]any{
				"confirm":   map[string]any{"type": "string", "enum": []string{"yes", "no", ""}},
				"source_id": map[string]any{"type": []string{"string", "null"}},
				"payments": map[string]any{
					"type": "array",
					"items": objectSchema(map[string]any{
						"source_id":   map[string]any{"type": []string{"string", "null"}},
						"amount_text": map[string]any{"type": "string"},
					}, "source_id", "amount_text"),
				},
				"category_id": map[string]any{"type": []string{"string", "null"}},
				"merchant":    map[string]any{"type": []string{"string", "null"}},
				"day_offset":  map[string]any{"type": []string{"integer", "null"}},
			},
		},
	}, "intent", "drafts", "correction", "undo_index", "period", "balance_source_id", "answer")

	var out struct {
		Intent string `json:"intent"`
		Drafts []struct {
			Type                string  `json:"type"`
			AmountText          string  `json:"amount_text"`
			Merchant            string  `json:"merchant"`
			SourceID            *string `json:"source_id"`
			DestinationSourceID *string `json:"destination_source_id"`
			CategoryID          *string `json:"category_id"`
			DayOffset           int     `json:"day_offset"`
		} `json:"drafts"`
		Correction *struct {
			TargetIndex int     `json:"target_index"`
			CategoryID  *string `json:"category_id"`
			SourceID    *string `json:"source_id"`
			Merchant    *string `json:"merchant"`
			AmountText  *string `json:"amount_text"`
			DayOffset   *int    `json:"day_offset"`
		} `json:"correction"`
		UndoIndex       int            `json:"undo_index"`
		Period          string         `json:"period"`
		BalanceSourceID *string        `json:"balance_source_id"`
		Answer          *answerPayload `json:"answer"`
	}
	if err := c.complete(ctx, intentSystem, user.String(), "intent", schema, &out); err != nil {
		return nil, err
	}

	in := &Intent{
		Kind:      normalizeKind(out.Intent),
		UndoIndex: max(out.UndoIndex, 0),
		Period:    normalizePeriod(out.Period),
		Answer:    decodeAnswer(out.Answer, req),
	}
	if id, ok := match(out.BalanceSourceID, req.Sources); ok {
		in.BalanceSource = id
	}
	for _, d := range out.Drafts {
		draft := IntentDraft{
			Type:       normalizeDraftType(d.Type),
			AmountText: strings.TrimSpace(d.AmountText),
			Merchant:   strings.TrimSpace(d.Merchant),
			DayOffset:  clampOffset(d.DayOffset),
		}
		if id, ok := match(d.SourceID, req.Sources); ok {
			draft.SourceID = id
		}
		if id, ok := match(d.DestinationSourceID, req.Sources); ok {
			draft.DestinationSourceID = id
		}
		if id, ok := match(d.CategoryID, req.Categories); ok {
			draft.CategoryID = id
		}
		in.Drafts = append(in.Drafts, draft)
	}
	if c := out.Correction; c != nil {
		corr := &Correction{TargetIndex: c.TargetIndex}
		if corr.TargetIndex < 0 || corr.TargetIndex > len(req.Recent) {
			corr.TargetIndex = 0
		}
		if id, ok := match(c.CategoryID, req.Categories); ok {
			corr.CategoryID = id
		}
		if id, ok := match(c.SourceID, req.Sources); ok {
			corr.SourceID = id
		}
		if c.Merchant != nil {
			if m := strings.TrimSpace(*c.Merchant); m != "" {
				corr.Merchant = &m
			}
		}
		if c.AmountText != nil {
			if a := strings.TrimSpace(*c.AmountText); a != "" {
				corr.AmountText = &a
			}
		}
		if c.DayOffset != nil {
			off := clampOffset(*c.DayOffset)
			corr.DayOffset = &off
		}
		in.Correction = corr
	}
	return in, nil
}

// normalizeKind enforces the enum: an unlisted kind is no answer at all.
func normalizeKind(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case KindCapture:
		return KindCapture
	case KindCorrect:
		return KindCorrect
	case KindUndo:
		return KindUndo
	case KindSummary:
		return KindSummary
	case KindBalance:
		return KindBalance
	case KindAnswer:
		return KindAnswer
	default:
		return KindUnknown
	}
}

func normalizeDraftType(typ string) string {
	switch strings.ToLower(strings.TrimSpace(typ)) {
	case "income":
		return "income"
	case "transfer":
		return "transfer"
	default:
		// A refund is a linked reversal and stays on the regex path; an
		// unknown type is an ordinary expense.
		return "expense"
	}
}

func normalizePeriod(period string) string {
	switch strings.ToLower(strings.TrimSpace(period)) {
	case "today":
		return "today"
	case "week":
		return "week"
	default:
		return ""
	}
}

// clampOffset keeps a relative date in the past and inside the memory
// window; anything else means today.
func clampOffset(offset int) int {
	if offset > 0 || offset < -MaxBackdateDays {
		return 0
	}
	return offset
}
