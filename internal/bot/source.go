package bot

import (
	"context"
	"regexp"

	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/llm"
	"github.com/google/uuid"
)

// Source bases, in the order resolveReceiptSource tries them. The basis
// is stored on the draft and decides the wording of the question: a
// remembered Source is stated with a hedge, a guessed one with a
// question mark, and a Source read off the paper as fact.
const (
	basisHint    = "hint"    // the photo's caption named it
	basisPrinted = "printed" // the receipt itself names the payment method
	basisMemory  = "memory"  // this merchant was last paid from there
	basisDefault = "default" // the Settings fallback, or the only live Source
	basisGuess   = "guess"   // the model repaired an alias we did not recognise
	basisUser    = "user"    // the user said so, in chat or in the dashboard
)

// sourcePick is a resolved Source plus how it was decided. A nil Source
// with an empty Basis is the one honest outcome left: ask.
type sourcePick struct {
	Source *entity.Source
	Basis  string
}

// cashLike matches a printed payment method that names physical money.
// It is deliberately never decisive: "TUNAI" is what a cash register
// prints for any settlement it does not itemise, so a card payment and a
// banknote look identical on the paper. Trusting it would file BCA
// spending under Cash — and the LLM repair call it used to trigger cost
// seven seconds for a wrong answer.
var cashLike = regexp.MustCompile(`(?i)\b(cash|tunai|kas)\b`)

// resolveReceiptSource decides which Source paid for a photographed
// receipt. First hit wins: the caption the user wrote, the method the
// paper printed, what this merchant was paid from last time, the photo
// default in Settings — and, failing all of those, nothing, which is a
// question rather than a guess (spec §6).
func (h *Harness) resolveReceiptSource(
	ctx context.Context,
	read *llm.ReceiptRead,
	captionHint *entity.Source,
	merchantID *uuid.UUID,
	sources []entity.Source,
) sourcePick {
	if captionHint != nil {
		return sourcePick{Source: captionHint, Basis: basisHint}
	}

	if read != nil && read.PaymentMethod != "" && !cashLike.MatchString(read.PaymentMethod) {
		if hit := resolveSourceRules(read.PaymentMethod, sources); hit != nil {
			return sourcePick{Source: hit, Basis: basisPrinted}
		}
		// An alias the rules do not know ("DEBIT BCA") is worth one model
		// call; a cash-like method never is.
		if hit := h.resolveSource(ctx, read.PaymentMethod, sources); hit != nil {
			return sourcePick{Source: hit, Basis: basisGuess}
		}
	}

	if merchantID != nil {
		id, err := h.txRepo.LastSourceForMerchant(ctx, *merchantID)
		if err != nil {
			h.log.Warn(traceOf(ctx), "source memory lookup failed", map[string]any{"error": err.Error()})
		}
		if id != nil {
			if hit := findSource(*id, sources); hit != nil {
				return sourcePick{Source: hit, Basis: basisMemory}
			}
		}
	}

	if id := h.photoDefaultSource(ctx); id != nil {
		if hit := findSource(*id, sources); hit != nil {
			return sourcePick{Source: hit, Basis: basisDefault}
		}
	}
	// Infer-if-unique: with a single spendable Source there is nothing to
	// ask about (spec §6).
	if hit := defaultSource(sources); hit != nil {
		return sourcePick{Source: hit, Basis: basisDefault}
	}
	return sourcePick{}
}

// photoDefaultSource reads the Settings fallback. A lookup failure is
// logged and treated as "not set": the question is a better outcome than
// a failed capture.
func (h *Harness) photoDefaultSource(ctx context.Context) *uuid.UUID {
	if h.settings == nil {
		return nil
	}
	id, err := h.settings.GetPhotoDefaultSource(ctx)
	if err != nil {
		h.log.Warn(traceOf(ctx), "photo default source lookup failed", map[string]any{"error": err.Error()})
		return nil
	}
	return id
}

// learnPhotoDefault remembers the first Source the user ever picks for a
// receipt, so the next photo can state it instead of asking. It only
// ever fills an empty setting: overwriting the user's own choice from
// Settings would make the fallback wander. Returns the learned Source,
// or nil when nothing was learned.
func (h *Harness) learnPhotoDefault(ctx context.Context, src *entity.Source) *entity.Source {
	if h.settings == nil || src == nil {
		return nil
	}
	current, err := h.settings.GetPhotoDefaultSource(ctx)
	if err != nil {
		h.log.Warn(traceOf(ctx), "photo default source lookup failed", map[string]any{"error": err.Error()})
		return nil
	}
	if current != nil {
		return nil
	}
	if err := h.settings.SetPhotoDefaultSource(ctx, &src.ID); err != nil {
		h.log.Warn(traceOf(ctx), "photo default source save failed", map[string]any{"error": err.Error()})
		return nil
	}
	return src
}
