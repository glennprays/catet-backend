package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/glennprays/catet-backend/internal/bot/tmpl"
	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/money"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/glennprays/catet-backend/internal/service"
	"github.com/google/uuid"
)

// billPaymentParams is the parked half of a `lunas` the bot could not
// finish in one message: which bills are still candidates, and whatever
// the user already said.
type billPaymentParams struct {
	BillID      *uuid.UUID  `json:"bill_id,omitempty"`
	Candidates  []uuid.UUID `json:"candidates,omitempty"`
	AmountMinor *int64      `json:"amount_minor,omitempty"`
	Chat        string      `json:"chat"`
	MessageID   string      `json:"message_id"`
}

// lunas settles a bill from chat. Bill CRUD stays on the dashboard
// (D15): the only thing chat can do to a bill is pay it, which is the
// one action that happens away from a screen.
func (h *Harness) lunas(
	ctx context.Context,
	chat, messageID string,
	state *repository.ChatState,
	query string,
	amount *int64,
	lang string,
) (string, []uuid.UUID, error) {
	candidates, err := h.lunasCandidates(ctx, query)
	if err != nil {
		return "", nil, err
	}

	switch len(candidates) {
	case 0:
		return tmpl.Render(lang, "lunas_none", nil), nil, nil
	case 1:
		return h.paySingleBill(ctx, chat, messageID, state, candidates[0], amount, lang)
	}

	ids := make([]uuid.UUID, 0, len(candidates))
	items := make([]string, 0, len(candidates))
	for i, b := range candidates {
		ids = append(ids, b.ID)
		items = append(items, fmt.Sprintf("%d. %s · %s", i+1, b.Name, formatDay(b.NextDueDate, lang)))
	}
	params, err := jsonParams(billPaymentParams{
		Candidates: ids, AmountMinor: amount, Chat: chat, MessageID: messageID,
	})
	if err != nil {
		return "", nil, err
	}
	action, err := h.pending.Create(ctx, "bill_payment", params, "Pilih tagihan")
	if err != nil {
		return "", nil, fmt.Errorf("park bill payment: %w", err)
	}
	expect := "bill_choice"
	state.OpenPendingActionID = &action.ID
	state.OpenExpect = &expect
	return tmpl.Render(lang, "lunas_choose", map[string]any{"Items": items}), nil, nil
}

// lunasCandidates answers "which bill did the user mean?". A bare
// `lunas` means one of the bills the user is currently being asked
// about — due, overdue, or named in today's digest. A named `lunas`
// searches every active bill, because paying early is legitimate.
func (h *Harness) lunasCandidates(ctx context.Context, query string) ([]entity.Bill, error) {
	bills, err := h.billRepo.ListActive(ctx)
	if err != nil {
		return nil, fmt.Errorf("list bills: %w", err)
	}
	if q := strings.ToLower(strings.TrimSpace(query)); q != "" {
		out := make([]entity.Bill, 0, 2)
		for _, b := range bills {
			if strings.Contains(strings.ToLower(b.Name), q) {
				out = append(out, b)
			}
		}
		return out, nil
	}

	today := service.JakartaToday()
	reminded, err := h.reminderSends.RemindedBillIDs(ctx, today)
	if err != nil {
		return nil, fmt.Errorf("list reminded bills: %w", err)
	}
	remindedSet := make(map[uuid.UUID]bool, len(reminded))
	for _, id := range reminded {
		remindedSet[id] = true
	}
	out := make([]entity.Bill, 0, 2)
	for _, b := range bills {
		if !b.NextDueDate.After(today) || remindedSet[b.ID] {
			out = append(out, b)
		}
	}
	return out, nil
}

// paySingleBill finishes the payment, or parks the one thing still
// missing. The bot never invents the amount: it fills it in only when
// the band leaves no choice (min == max), and asks otherwise (D12).
func (h *Harness) paySingleBill(
	ctx context.Context,
	chat, messageID string,
	state *repository.ChatState,
	bill entity.Bill,
	amount *int64,
	lang string,
) (string, []uuid.UUID, error) {
	if amount == nil && bill.AmountMinMinor == bill.AmountMaxMinor {
		fixed := bill.AmountMinMinor
		amount = &fixed
	}
	if amount == nil {
		return h.parkBillPayment(ctx, chat, messageID, state, bill, nil, "amount", "lunas_amount",
			map[string]any{"Name": bill.Name}, lang)
	}
	if bill.LinkedSourceID == nil {
		sources, err := h.sources.List(ctx, false)
		if err != nil {
			return "", nil, fmt.Errorf("list sources: %w", err)
		}
		return h.parkBillPayment(ctx, chat, messageID, state, bill, amount, "source", "lunas_source",
			map[string]any{"Name": bill.Name, "Sources": sourceNames(sources)}, lang)
	}
	return h.payBillNow(ctx, state, messageID, bill.ID, *amount, nil, lang)
}

// payBillNow writes the expense and advances the bill, then closes any
// question that was open for it.
func (h *Harness) payBillNow(
	ctx context.Context,
	state *repository.ChatState,
	messageID string,
	billID uuid.UUID,
	amount int64,
	sourceID *uuid.UUID,
	lang string,
) (string, []uuid.UUID, error) {
	data, err := h.bills.Pay(ctx, billID, service.PayBillInput{
		AmountMinor: amount,
		SourceID:    sourceID,
		Origin:      botOrigin,
	})
	if err != nil {
		return "", nil, err
	}
	// This message owns the batch `batal` addresses: undoing a payment is
	// the same gesture as undoing a capture.
	if messageID != "" {
		state.LastBatchMessageID = &messageID
	}
	next := tmpl.Render(lang, "lunas_completed", nil)
	if data.Bill.Active {
		next = formatDay(data.Bill.NextDueDate, lang)
	}
	reply := tmpl.Render(lang, "lunas_done", map[string]any{
		"Name":   data.Bill.Name,
		"Amount": money.FormatIDR(amount),
		"Next":   next,
	})
	return reply, []uuid.UUID{data.Transaction.ID}, nil
}

// parkBillPayment stores what the bot already knows and asks for the
// single missing field (ADR-0003: one question, never a guess).
func (h *Harness) parkBillPayment(
	ctx context.Context,
	chat, messageID string,
	state *repository.ChatState,
	bill entity.Bill,
	amount *int64,
	expect, question string,
	data map[string]any,
	lang string,
) (string, []uuid.UUID, error) {
	id := bill.ID
	params, err := jsonParams(billPaymentParams{
		BillID: &id, AmountMinor: amount, Chat: chat, MessageID: messageID,
	})
	if err != nil {
		return "", nil, err
	}
	action, err := h.pending.Create(ctx, "bill_payment", params, "Lunas "+bill.Name)
	if err != nil {
		return "", nil, fmt.Errorf("park bill payment: %w", err)
	}
	state.OpenPendingActionID = &action.ID
	state.OpenExpect = &expect
	return tmpl.Render(lang, question, data), nil, nil
}

// answerBillPayment resumes a parked `lunas`: a number picks a
// candidate, an amount fills the blank, a name picks the Source.
func (h *Harness) answerBillPayment(
	ctx context.Context,
	edits *answerEdits,
	pending *entity.PendingAction,
	state *repository.ChatState,
	expect string,
	lang string,
) (string, []uuid.UUID, bool, error) {
	var params billPaymentParams
	if err := json.Unmarshal(pending.Params, &params); err != nil {
		return "", nil, false, fmt.Errorf("decode bill payment params: %w", err)
	}

	switch expect {
	case "bill_choice":
		if edits.Choice == nil {
			return "", nil, false, nil
		}
		idx := *edits.Choice - 1
		if idx < 0 || idx >= len(params.Candidates) {
			return "", nil, false, nil
		}
		bill, err := h.billRepo.Get(ctx, params.Candidates[idx])
		if err != nil {
			return "", nil, false, fmt.Errorf("load bill: %w", err)
		}
		if bill == nil {
			return "", nil, false, nil
		}
		if _, err := h.pending.Resolve(ctx, pending.ID, "confirm"); err != nil {
			return "", nil, false, fmt.Errorf("resolve bill payment: %w", err)
		}
		if err := h.clearQuestion(ctx, state); err != nil {
			return "", nil, false, err
		}
		reply, ids, err := h.paySingleBill(ctx, params.Chat, params.MessageID, state, *bill, params.AmountMinor, lang)
		if err != nil {
			return "", nil, false, err
		}
		// paySingleBill may have opened the next question (amount, source).
		return reply, ids, true, h.saveState(ctx, state)

	case "amount":
		if edits.AmountMinor == nil || params.BillID == nil {
			return "", nil, false, nil
		}
		bill, err := h.billRepo.Get(ctx, *params.BillID)
		if err != nil {
			return "", nil, false, fmt.Errorf("load bill: %w", err)
		}
		if bill == nil {
			return "", nil, false, nil
		}
		if _, err := h.pending.Resolve(ctx, pending.ID, "confirm"); err != nil {
			return "", nil, false, fmt.Errorf("resolve bill payment: %w", err)
		}
		if err := h.clearQuestion(ctx, state); err != nil {
			return "", nil, false, err
		}
		reply, ids, err := h.paySingleBill(ctx, params.Chat, params.MessageID, state, *bill, edits.AmountMinor, lang)
		if err != nil {
			return "", nil, false, err
		}
		return reply, ids, true, h.saveState(ctx, state)

	case "source":
		if edits.SourceID == nil || params.BillID == nil || params.AmountMinor == nil {
			return "", nil, false, nil
		}
		if _, err := h.pending.Resolve(ctx, pending.ID, "confirm"); err != nil {
			return "", nil, false, fmt.Errorf("resolve bill payment: %w", err)
		}
		if err := h.clearQuestion(ctx, state); err != nil {
			return "", nil, false, err
		}
		reply, ids, err := h.payBillNow(ctx, state, params.MessageID, *params.BillID, *params.AmountMinor, edits.SourceID, lang)
		return reply, ids, true, err
	}
	return "", nil, false, nil
}
