package bot

import (
	"context"
	"strings"

	"github.com/glennprays/catet-backend/internal/bot/parse"
	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/llm"
	"github.com/google/uuid"
)

// matchSources returns every Source whose name equals the hint or
// contains it as a whole word ("bca" → "BCA Tahapan"). Substring
// matching is deliberately excluded: "cash" must not match "Cashback".
func matchSources(hint string, sources []entity.Source) []entity.Source {
	needle := strings.ToLower(strings.TrimSpace(hint))
	if needle == "" {
		return nil
	}
	var exact, partial []entity.Source
	for _, s := range sources {
		name := strings.ToLower(s.Name)
		if name == needle {
			exact = append(exact, s)
			continue
		}
		for _, w := range strings.FieldsFunc(name, func(r rune) bool {
			return r == ' ' || r == '-' || r == '_' || r == '/'
		}) {
			if w == needle {
				partial = append(partial, s)
				break
			}
		}
	}
	if len(exact) > 0 {
		return exact
	}
	return partial
}

// resolveSourceRules is the deterministic half of Source resolution: a
// unique name match, or nothing.
func resolveSourceRules(hint string, sources []entity.Source) *entity.Source {
	hits := matchSources(hint, sources)
	if len(hits) == 1 {
		return &hits[0]
	}
	return nil
}

// matchCategories returns every category whose name equals the hint or
// contains it as a whole word ("date" → "Date & Social"), the same law
// matchSources applies to Source names.
func matchCategories(hint string, cats []entity.Category) []entity.Category {
	needle := strings.ToLower(strings.TrimSpace(hint))
	if needle == "" {
		return nil
	}
	var exact, partial []entity.Category
	for _, c := range cats {
		name := strings.ToLower(c.Name)
		if name == needle {
			exact = append(exact, c)
			continue
		}
		for _, w := range strings.FieldsFunc(name, func(r rune) bool {
			return r == ' ' || r == '-' || r == '_' || r == '/'
		}) {
			if w == needle {
				partial = append(partial, c)
				break
			}
		}
	}
	if len(exact) > 0 {
		return exact
	}
	return partial
}

// resolveCategoryRules is the deterministic half of category resolution:
// a unique name match, or nothing.
func resolveCategoryRules(hint string, cats []entity.Category) *entity.Category {
	hits := matchCategories(hint, cats)
	if len(hits) == 1 {
		return &hits[0]
	}
	return nil
}

// resolveSource applies the rules first and only then asks the LLM to
// repair a typo'd alias. Whatever the model returns must be one of the
// whitelisted ids (ADR-0003) or it is discarded.
func (h *Harness) resolveSource(ctx context.Context, hint string, sources []entity.Source) *entity.Source {
	if hit := resolveSourceRules(hint, sources); hit != nil {
		return hit
	}
	if !optionalAuxiliaryEnrichmentEnabled(ctx) || hint == "" {
		return nil
	}
	modelCtx, cancel, ok := h.auxiliaryModelContext(ctx)
	if !ok {
		return nil
	}
	defer cancel()
	if !h.llm.Enabled(modelCtx) {
		return nil
	}
	cands := make([]llm.Candidate, 0, len(sources))
	for _, s := range sources {
		cands = append(cands, llm.Candidate{ID: s.ID, Name: s.Name, Hint: s.Type})
	}
	id, err := h.llm.PickSource(modelCtx, hint, cands)
	if err != nil {
		h.log.Warn(traceOf(ctx), "llm source pick failed", map[string]any{"error": err.Error()})
		return nil
	}
	if id == nil {
		return nil
	}
	for i := range sources {
		if sources[i].ID == *id {
			return &sources[i]
		}
	}
	return nil
}

// draftSource applies the Source law to a typed capture: the rules try
// every clause first; only an unmatched pakai-clause is worth a model
// call, because "dari X" may just as well be the shop. explicit reports
// whether the user named a Source at all, which is what switches the
// memory/default fallbacks off. places keeps the clauses that named a
// location, in message order — the first one is the merchant.
func (h *Harness) draftSource(
	ctx context.Context,
	d draft,
	sources []entity.Source,
) (src *entity.Source, places []string, explicit bool) {
	for _, word := range d.Pays {
		if src != nil {
			break
		}
		src = resolveSourceRules(word, sources)
	}
	// A refund's "dari" names the shop it came back from, never a Source.
	for _, word := range d.Froms {
		if d.Type == parse.TypeRefund {
			places = append(places, word)
			continue
		}
		hit := resolveSourceRules(word, sources)
		if hit == nil {
			places = append(places, word)
			continue
		}
		if src == nil {
			src = hit
		}
	}
	places = append(places, d.Places...)

	// Intent Source IDs augment deterministic parser hints; they never
	// erase a matching pakai/dari clause.
	if src == nil && d.SourceID != nil {
		if hit := findSource(*d.SourceID, sources); hit != nil {
			src = hit
		}
	}

	explicit = len(d.Pays) > 0 || src != nil
	if src == nil && len(d.Pays) > 0 {
		// "pake bcaa" can only have meant a Source, so one repair call
		// is justified. A "dari" clause never reaches the model.
		src = h.resolveSource(ctx, d.Pays[0], sources)
	}
	return src, places, explicit
}

// defaultSource is the "infer-if-unique" rule (spec §6): with a single
// spendable Source there is nothing to ask about.
func defaultSource(sources []entity.Source) *entity.Source {
	var live []entity.Source
	for _, s := range sources {
		if !s.IsDebt {
			live = append(live, s)
		}
	}
	if len(live) == 1 {
		return &live[0]
	}
	if len(sources) == 1 {
		return &sources[0]
	}
	return nil
}

// categoryVerdict is the resolved label for one draft plus the
// provenance the ledger must record.
type categoryVerdict struct {
	ID           *uuid.UUID
	Name         string
	Source       *string
	Confidence   *float64
	ReviewStatus string
}

// resolveCategory prefers the user's own history over the model:
// a category the user set on this merchant is a rule, not a guess. An
// LLM label below the confidence threshold still writes, but lands in
// Needs-Review; no label at all writes uncategorised, also in
// Needs-Review. merchantID is the canonical merchant (nil when the
// capture named none); merchant is its display name, for the prompt.
func (h *Harness) resolveCategory(
	ctx context.Context, merchantID *uuid.UUID, merchant, note, typ string, cats []entity.Category,
) categoryVerdict {
	needsReview := categoryVerdict{ReviewStatus: "needs_review"}
	if merchantID != nil {
		id, err := h.txRepo.LastUserCategoryForMerchant(ctx, *merchantID)
		if err != nil {
			h.log.Warn(traceOf(ctx), "merchant memory lookup failed", map[string]any{"error": err.Error()})
		}
		if id != nil {
			src := "rules"
			conf := 1.0
			return categoryVerdict{
				ID: id, Name: categoryName(*id, cats), Source: &src,
				Confidence: &conf, ReviewStatus: "clean",
			}
		}
	}
	if !h.llm.Enabled(ctx) || len(cats) == 0 {
		return needsReview
	}
	modelCtx, cancel, ok := h.categoryModelContext(ctx)
	if !ok {
		return needsReview
	}
	defer cancel()

	cands := make([]llm.Candidate, 0, len(cats))
	for _, c := range cats {
		cands = append(cands, llm.Candidate{ID: c.ID, Name: c.Name, Hint: parentName(c, cats)})
	}
	id, confidence, err := h.llm.PickCategory(modelCtx, merchant, note, typ, cands)
	if err != nil {
		h.log.Warn(traceOf(ctx), "llm category pick failed", map[string]any{"error": err.Error()})
		return needsReview
	}
	if id == nil {
		return needsReview
	}
	src := "llm"
	conf := confidence
	status := "clean"
	if confidence < categoryReviewThreshold {
		status = "needs_review"
	}
	return categoryVerdict{
		ID: id, Name: categoryName(*id, cats), Source: &src,
		Confidence: &conf, ReviewStatus: status,
	}
}

func categoryName(id uuid.UUID, cats []entity.Category) string {
	for _, c := range cats {
		if c.ID == id {
			return c.Name
		}
	}
	return ""
}

func parentName(c entity.Category, cats []entity.Category) string {
	if c.ParentID == nil {
		return ""
	}
	return categoryName(*c.ParentID, cats)
}

// sourceNameOf resolves a display name for a source id.
func sourceNameOf(id uuid.UUID, sources []entity.Source) string {
	for _, s := range sources {
		if s.ID == id {
			return s.Name
		}
	}
	return ""
}

// findSource looks up a whitelisted id in the loaded set. An id that is
// not there (archived between the list and the write) is no answer.
func findSource(id uuid.UUID, sources []entity.Source) *entity.Source {
	for i := range sources {
		if sources[i].ID == id {
			return &sources[i]
		}
	}
	return nil
}
