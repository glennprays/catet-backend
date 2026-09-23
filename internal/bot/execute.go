package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/glennprays/catet-backend/internal/apperror"
	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/service"
	"github.com/google/uuid"
)

// Execute performs a confirmed Hold Queue action. It is the single
// execution path shared by the chat ("ya", or an answered question) and
// the dashboard's Konfirmasi button, so a parked draft resolves the same
// way whoever confirms it. The pending row is resolved in the same call:
// an executed action must never stay open.
//
// It returns every transaction the action wrote: one for a capture or a
// refund, one per line for a photographed receipt.
func (h *Harness) Execute(ctx context.Context, p *entity.PendingAction, ov *service.PendingOverrides) ([]*entity.Transaction, error) {
	if p == nil {
		return nil, apperror.NotFound("pending action not found")
	}

	var (
		txs []*entity.Transaction
		err error
	)
	switch p.Kind {
	case "capture", "create_transaction":
		var tx *entity.Transaction
		tx, err = h.executeCapture(ctx, p, ov)
		if tx != nil {
			txs = []*entity.Transaction{tx}
		}
	case "refund":
		var params refundParams
		if unmarshalErr := json.Unmarshal(p.Params, &params); unmarshalErr != nil {
			err = unmarshalErr
			break
		}
		existing, lookupErr := h.txRepo.GetReversalByOriginalID(ctx, params.ReversesTransactionID)
		if lookupErr != nil {
			err = lookupErr
			break
		}
		if existing != nil {
			txs = []*entity.Transaction{existing}
			break
		}
		var tx *entity.Transaction
		tx, err = h.executeRefund(ctx, p, ov)
		if tx != nil {
			txs = []*entity.Transaction{tx}
		}
	case "receipt":
		txs, err = h.executeReceipt(ctx, p, ov)
	default:
		return nil, apperror.BadRequestf("pending action kind %q cannot be executed", p.Kind)
	}
	if err != nil {
		return txs, err
	}

	ok, err := h.pending.Resolve(ctx, p.ID, "confirm")
	if err != nil {
		if recovered, lookupErr := h.recoverPendingTransactions(ctx, p, ov); lookupErr == nil && len(recovered) > 0 {
			txs = recovered
		}
		return txs, apperror.Internal("failed to resolve pending action").Wrap(err)
	}
	if !ok {
		return txs, apperror.NotFound("pending action not found")
	}
	return txs, nil
}

func (h *Harness) recoverPendingTransactions(ctx context.Context, p *entity.PendingAction, ov *service.PendingOverrides) ([]*entity.Transaction, error) {
	var externalIDs []string
	switch p.Kind {
	case "capture", "create_transaction":
		var params captureParams
		if err := json.Unmarshal(p.Params, &params); err != nil { return nil, err }
		if params.ExternalID != "" { externalIDs = append(externalIDs, params.ExternalID) }
	case "receipt":
		var params receiptParams
		if err := json.Unmarshal(p.Params, &params); err != nil { return nil, err }
		if ov != nil && ov.Collapse != nil && *ov.Collapse {
			externalIDs = append(externalIDs, receiptExternalID(params.MessageID, 0))
		} else {
			for _, item := range params.Items { externalIDs = append(externalIDs, receiptExternalID(params.MessageID, item.N)+item.Part) }
		}
	case "refund":
		var params refundParams
		if err := json.Unmarshal(p.Params, &params); err != nil { return nil, err }
		tx, err := h.txRepo.GetReversalByOriginalID(ctx, params.ReversesTransactionID)
		if err != nil { return nil, err }
		if tx != nil { return []*entity.Transaction{tx}, nil }
	}
	out := make([]*entity.Transaction, 0, len(externalIDs))
	for _, externalID := range externalIDs {
		tx, err := h.txRepo.GetByExternalID(ctx, externalID)
		if err != nil { return nil, err }
		if tx != nil { out = append(out, tx) }
	}
	return out, nil
}

func (h *Harness) executeCapture(ctx context.Context, p *entity.PendingAction, ov *service.PendingOverrides) (*entity.Transaction, error) {
	var params captureParams
	if err := json.Unmarshal(p.Params, &params); err != nil {
		return nil, apperror.Internal("failed to decode pending capture").Wrap(err)
	}

	in := service.CreateInput{
		Type:                params.Type,
		AmountMinor:         params.AmountMinor,
		DestinationSourceID: params.DestinationSourceID,
		CategoryID:          params.CategoryID,
		CategorySource:      params.CategorySource,
		CategoryConfidence:  params.CategoryConfidence,
		Origin:              botOrigin,
	}
	occurred := params.OccurredAt
	in.OccurredAt = &occurred
	if params.SourceID != nil {
		in.SourceID = *params.SourceID
	}
	if params.Merchant != "" {
		merchant := params.Merchant
		in.Merchant = &merchant
	}
	in.MerchantID = params.MerchantID
	if params.Note != "" && (ov == nil || ov.Note == nil) {
		note := params.Note
		in.Note = &note
	}
	if params.ExternalID != "" {
		external := params.ExternalID
		in.ExternalID = &external
	}

	if ov != nil {
		if ov.AmountMinor != nil {
			in.AmountMinor = *ov.AmountMinor
		}
		if ov.SourceID != nil {
			in.SourceID = *ov.SourceID
		}
		if ov.DestinationSourceID != nil {
			in.DestinationSourceID = ov.DestinationSourceID
		}
		if ov.CategoryID != nil {
			// A human picked this label; it outranks whatever the rules or
			// the model had stored.
			in.CategoryID = ov.CategoryID
			user := "user"
			in.CategorySource = &user
			in.CategoryConfidence = nil
			in.ReviewStatus = "clean"
		}
		if ov.Merchant != nil {
			// A human typed this name in the dashboard: it is authoritative
			// as written, and it re-points the row's merchant entity.
			in.Merchant = ov.Merchant
			in.MerchantID = nil
			m, err := h.merchants.ResolveUser(ctx, *ov.Merchant)
			if err != nil {
				return nil, err
			}
			if m != nil {
				in.MerchantID = &m.ID
			}
		}
		if ov.Note != nil {
			in.Note = ov.Note
		}
		if ov.OccurredDate != nil && *ov.OccurredDate != "" {
			in.OccurredAt = nil
			in.OccurredDate = ov.OccurredDate
		}
	}

	// A parked capture reaches the ledger without ever passing through
	// the inline category resolution, so label it now — otherwise
	// answering "which source?" writes an uncategorised row the user
	// already described ("ketang goreng" → belum ada kategori).
	if in.CategoryID == nil && (in.Type == "expense" || in.Type == "income") {
		cats, err := h.categories.List(ctx, false)
		if err != nil {
			return nil, apperror.Internal("failed to list categories").Wrap(err)
		}
		merchant := ""
		if in.Merchant != nil {
			merchant = *in.Merchant
		}
		verdict := h.resolveCategory(ctx, in.MerchantID, merchant, params.Note, in.Type, cats)
		in.CategoryID = verdict.ID
		in.CategorySource = verdict.Source
		in.CategoryConfidence = verdict.Confidence
		in.ReviewStatus = verdict.ReviewStatus
	}

	tx, err := h.txs.Create(ctx, in)
	if err != nil && errIsConflict(err) && params.ExternalID != "" {
		if existing, getErr := h.txRepo.GetByExternalID(ctx, params.ExternalID); getErr == nil && existing != nil {
			tx, err = existing, nil
		}
	}
	if err != nil {
		return nil, err
	}
	// The message that asked the question now carries its outcome.
	h.react(ctx, params.Chat, params.MessageID, params.From, reactWritten)
	return tx, nil
}

func (h *Harness) executeRefund(ctx context.Context, p *entity.PendingAction, _ *service.PendingOverrides) (*entity.Transaction, error) {
	var params refundParams
	if err := json.Unmarshal(p.Params, &params); err != nil {
		return nil, apperror.Internal("failed to decode pending refund").Wrap(err)
	}
	note := fmt.Sprintf("refund %s via WhatsApp", params.Merchant)
	tx, err := h.txs.Reverse(ctx, params.ReversesTransactionID, &note, botOrigin)
	if err != nil {
		return nil, err
	}
	h.react(ctx, params.Chat, params.MessageID, params.From, reactWritten)
	return tx, nil
}

// executeReceipt writes one ledger line per receipt item. The lines are
// written sequentially and each carries its own external_id, so a
// confirmation that failed halfway through is finished — never doubled —
// by the next attempt.
func (h *Harness) executeReceipt(
	ctx context.Context, p *entity.PendingAction, ov *service.PendingOverrides,
) ([]*entity.Transaction, error) {
	var params receiptParams
	if err := json.Unmarshal(p.Params, &params); err != nil {
		return nil, apperror.Internal("failed to decode pending receipt").Wrap(err)
	}

	merchant := params.Merchant
	var (
		userCategory *uuid.UUID
		lineCategory map[int]uuid.UUID
		occurredDate *string
		noteSuffix   string
	)
	if ov != nil {
		// A receipt's money is the printed total; changing it would make
		// the lines disagree with the paper. Per-line edits happen in the
		// ledger, after the write.
		if ov.AmountMinor != nil {
			return nil, apperror.Validation("transaction validation failed").WithDetails(apperror.FieldError{
				Field: "amount_minor", Rule: "immutable",
				Message: "Nominal struk tidak bisa diubah, ubah per baris setelah tersimpan",
			})
		}
		if ov.SourceID != nil {
			params.SourceID = ov.SourceID
			// A Source chosen in the dashboard replaces a split: the user
			// is saying the whole bill came from one account.
			params.Payments = nil
			for i := range params.Items {
				params.Items[i].SourceID = nil
				params.Items[i].SourceName = ""
			}
		}
		if ov.DestinationSourceID != nil {
			params.DestinationSourceID = ov.DestinationSourceID
		}
		if ov.CategoryID != nil {
			userCategory = ov.CategoryID
		}
		if ov.Merchant != nil {
			merchant = *ov.Merchant
		}
		if ov.Note != nil {
			noteSuffix = *ov.Note
		}
		if ov.OccurredDate != nil && *ov.OccurredDate != "" {
			occurredDate = ov.OccurredDate
		}
		// Collapsing writes a single line, so there is no line left for a
		// per-line correction to name: the two overrides contradict.
		if ov.Collapse != nil && *ov.Collapse && len(ov.Items) > 0 {
			return nil, apperror.Validation("transaction validation failed").WithDetails(apperror.FieldError{
				Field: "items", Rule: "conflict",
				Message: "Rincian per baris tidak berlaku untuk satu baris",
			})
		}
		// Per-line labels are keyed by the parked line's N, so a stale
		// sheet cannot silently relabel the wrong line: an N the draft
		// does not have is refused, never ignored.
		for _, it := range ov.Items {
			if it.CategoryID == nil {
				continue
			}
			found := false
			for j := range params.Items {
				if params.Items[j].N == it.N {
					found = true
					break
				}
			}
			if !found {
				return nil, apperror.Validation("transaction validation failed").WithDetails(apperror.FieldError{
					Field: "items", Rule: "unknown_line",
					Message: fmt.Sprintf("Baris %d tidak ada", it.N),
				})
			}
			if lineCategory == nil {
				lineCategory = make(map[int]uuid.UUID, len(ov.Items))
			}
			lineCategory[it.N] = *it.CategoryID
		}
		// The dashboard's opt-out of the per-item split: one line at the
		// printed total. N is 0 — no parked line ever uses it, so the
		// collapsed write can never adopt a half-written per-line attempt.
		if ov.Collapse != nil && *ov.Collapse && len(params.Items) > 1 {
			id, conf := majorityLineCategory(params.Items)
			label := ""
			if id != nil {
				for _, it := range params.Items {
					if it.CategoryID != nil && *it.CategoryID == *id {
						label = it.CategoryName
						break
					}
				}
			}
			params.Items = []receiptItem{{
				N:                  0,
				Name:               merchant + " · " + strconv.Itoa(len(params.Items)) + " item",
				AmountMinor:        params.TotalMinor,
				CategoryID:         id,
				CategoryConfidence: conf,
				CategoryName:       label,
			}}
		}
	}
	if params.SourceID == nil {
		return nil, apperror.Validation("transaction validation failed").WithDetails(apperror.FieldError{
			Field: "source_id", Rule: "required", Message: "Pilih sumber dana",
		})
	}

	out := make([]*entity.Transaction, 0, len(params.Items))
	for _, item := range params.Items {
		occurred := params.OccurredAt
		note := item.Name
		if noteSuffix != "" {
			note += " — " + noteSuffix
		}
		externalID := receiptExternalID(params.MessageID, item.N) + item.Part
		source := *params.SourceID
		if item.SourceID != nil {
			// A split bill: this line was paid from its own Source.
			source = *item.SourceID
		}
		in := service.CreateInput{
			Type:                params.Type,
			OccurredAt:          &occurred,
			AmountMinor:         item.AmountMinor,
			SourceID:            source,
			DestinationSourceID: params.DestinationSourceID,
			Merchant:            &merchant,
			Note:                &note,
			Origin:              photoOrigin,
			ReviewStatus:        "clean",
			ExternalID:          &externalID,
			ContentHash:         &params.ContentHash,
		}
		// A backdate from the sheet moves the whole receipt: every line
		// is the same purchase.
		if occurredDate != nil {
			in.OccurredAt = nil
			in.OccurredDate = occurredDate
		}
		// A transfer has no category: both legs are the user's own money.
		if params.Type != "transfer" {
			lineID, hasLine := lineCategory[item.N]
			switch {
			case hasLine:
				// The sheet labelled this exact line; it outranks a
				// whole-receipt override and the model's own guess.
				user := "user"
				in.CategoryID = &lineID
				in.CategorySource = &user
			case userCategory != nil:
				user := "user"
				in.CategoryID = userCategory
				in.CategorySource = &user
			case params.CategorySource == "user" && item.CategoryID != nil:
				// The user labelled the whole receipt in chat; that is a
				// decision, not a guess, so nothing needs reviewing.
				user := "user"
				in.CategoryID = item.CategoryID
				in.CategorySource = &user
			case item.CategoryID != nil:
				llmSource := "llm"
				in.CategoryID = item.CategoryID
				in.CategorySource = &llmSource
				in.CategoryConfidence = item.CategoryConfidence
				if item.CategoryConfidence == nil || *item.CategoryConfidence < categoryReviewThreshold {
					in.ReviewStatus = "needs_review"
				}
			default:
				in.ReviewStatus = "needs_review"
			}
		}

		tx, err := h.txs.Create(ctx, in)
		if err != nil {
			if !errIsConflict(err) {
				// The lines already written stay written; the pending row
				// stays open, so the next confirmation finishes the rest.
				return out, err
			}
			// An earlier partial attempt already wrote this line: adopt it
			// instead of writing a second copy.
			existing, getErr := h.txRepo.GetByExternalID(ctx, externalID)
			if getErr != nil {
				return out, apperror.Internal("failed to load written receipt line").Wrap(getErr)
			}
			if existing == nil {
				return out, err
			}
			h.log.Warn(traceOf(ctx), "receipt line already written", map[string]any{"external_id": externalID})
			out = append(out, existing)
			continue
		}
		out = append(out, tx)
	}

	// The photo now carries its own outcome: the money exists.
	h.react(ctx, params.Chat, params.MessageID, params.From, reactWritten)
	// A Source picked in the dashboard teaches the same fallback a chat
	// answer does, silently — there is no reply to say it in.
	if ov != nil && ov.SourceID != nil {
		sources, err := h.sources.List(ctx, false)
		if err == nil {
			h.learnPhotoDefault(ctx, findSource(*ov.SourceID, sources))
		}
	}
	return out, nil
}
