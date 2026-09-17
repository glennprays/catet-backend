package service

import (
	"context"
	"errors"
	"strings"
	"unicode"

	"github.com/glennprays/catet-backend/internal/apperror"
	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/llm"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/glennprays/log"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// merchantConfidenceFloor is how sure the model must be before a raw
// phrase is folded into an existing merchant. Below it the phrase gets
// its own tidied merchant, which the user can merge in Settings — a
// wrong merge is much more expensive to notice than a duplicate row.
const merchantConfidenceFloor = 0.75

// merchantCandidateLimit caps the whitelist handed to the model.
const merchantCandidateLimit = 300

// merchantNameMaxLen bounds a user-supplied name; the column is text,
// but a display label that long is a paste accident.
const merchantNameMaxLen = 80

// MerchantService turns the many ways a merchant gets spelled into one
// canonical entity per user. Alias memory is the point: the model is
// asked about each raw phrase exactly once, ever.
type MerchantService struct {
	merchants *repository.MerchantRepository
	llm       *llm.Client
	log       *log.Logger
}

func NewMerchantService(
	merchants *repository.MerchantRepository,
	llmClient *llm.Client,
	logger *log.Logger,
) *MerchantService {
	return &MerchantService{merchants: merchants, llm: llmClient, log: logger}
}

// Resolve turns a raw phrase into the user's canonical Merchant: alias
// memory first, then an exact name, then one model call, and when the
// model is off or fails the phrase itself, Title-Cased, so a capture is
// never blocked by naming. Empty phrase → nil, nil.
func (s *MerchantService) Resolve(ctx context.Context, raw, note string) (*entity.Merchant, error) {
	key := repository.AliasKey(raw)
	if key == "" {
		return nil, nil
	}
	if m, err := s.remembered(ctx, key, raw); err != nil || m != nil {
		return m, err
	}

	if s.llm.Enabled(ctx) {
		m, err := s.resolveWithModel(ctx, key, raw, note)
		if err == nil {
			return m, nil
		}
		s.log.Warn(txTraceOf(ctx), "merchant resolve degraded", map[string]any{"error": err.Error()})
	}

	m, err := s.findOrCreate(ctx, titleCase(raw))
	if err != nil {
		return nil, err
	}
	if err := s.merchants.PutAlias(ctx, key, m.ID, "raw"); err != nil {
		return nil, apperror.Internal("failed to remember merchant").Wrap(err)
	}
	return m, nil
}

// ResolveUser is Resolve without the model: text the user chose on
// purpose (a correction, a form field, a rename) is authoritative as
// typed.
func (s *MerchantService) ResolveUser(ctx context.Context, text string) (*entity.Merchant, error) {
	key := repository.AliasKey(text)
	if key == "" {
		return nil, nil
	}
	if m, err := s.remembered(ctx, key, text); err != nil || m != nil {
		return m, err
	}
	m, err := s.findOrCreate(ctx, titleCase(text))
	if err != nil {
		return nil, err
	}
	if err := s.merchants.PutAlias(ctx, key, m.ID, "user"); err != nil {
		return nil, apperror.Internal("failed to remember merchant").Wrap(err)
	}
	return m, nil
}

// Lookup is memory only (alias, then exact name) with no side effects;
// for read paths such as picking a refund target.
func (s *MerchantService) Lookup(ctx context.Context, raw string) (*entity.Merchant, error) {
	key := repository.AliasKey(raw)
	if key == "" {
		return nil, nil
	}
	m, err := s.merchants.FindByAlias(ctx, key)
	if err != nil {
		return nil, apperror.Internal("failed to look up merchant").Wrap(err)
	}
	if m != nil {
		return m, nil
	}
	m, err = s.merchants.FindByName(ctx, key)
	if err != nil {
		return nil, apperror.Internal("failed to look up merchant").Wrap(err)
	}
	return m, nil
}

// remembered answers from memory alone: the alias for this exact phrase,
// or a merchant already carrying this name. A name hit is recorded as a
// 'rules' alias so the same phrase never costs a lookup twice.
func (s *MerchantService) remembered(ctx context.Context, key, raw string) (*entity.Merchant, error) {
	m, err := s.merchants.FindByAlias(ctx, key)
	if err != nil {
		return nil, apperror.Internal("failed to look up merchant").Wrap(err)
	}
	if m != nil {
		return m, nil
	}
	m, err = s.merchants.FindByName(ctx, raw)
	if err != nil {
		return nil, apperror.Internal("failed to look up merchant").Wrap(err)
	}
	if m == nil {
		return nil, nil
	}
	if err := s.merchants.PutAlias(ctx, key, m.ID, "rules"); err != nil {
		return nil, apperror.Internal("failed to remember merchant").Wrap(err)
	}
	return m, nil
}

// resolveWithModel asks the model once and records the answer as an
// alias. A returned error means the capture should fall back to the raw
// phrase; it is never surfaced to the user.
func (s *MerchantService) resolveWithModel(ctx context.Context, key, raw, note string) (*entity.Merchant, error) {
	cands, err := s.merchants.Candidates(ctx, merchantCandidateLimit)
	if err != nil {
		return nil, err
	}
	known := make([]llm.Candidate, 0, len(cands))
	for _, c := range cands {
		known = append(known, llm.Candidate{ID: c.ID, Name: c.Name})
	}

	pick, err := s.llm.PickMerchant(ctx, raw, note, known)
	if err != nil {
		return nil, err
	}

	if pick.MerchantID != nil && pick.Confidence >= merchantConfidenceFloor {
		m, err := s.merchants.Get(ctx, *pick.MerchantID)
		if err != nil {
			return nil, err
		}
		if m != nil {
			if err := s.merchants.PutAlias(ctx, key, m.ID, "llm"); err != nil {
				return nil, err
			}
			return m, nil
		}
	}

	name := pick.Name
	if strings.TrimSpace(name) == "" {
		name = titleCase(raw)
	}
	m, err := s.findOrCreate(ctx, name)
	if err != nil {
		return nil, err
	}
	if err := s.merchants.PutAlias(ctx, key, m.ID, "llm"); err != nil {
		return nil, err
	}
	return m, nil
}

func (s *MerchantService) findOrCreate(ctx context.Context, name string) (*entity.Merchant, error) {
	m, err := s.merchants.FindByName(ctx, name)
	if err != nil {
		return nil, apperror.Internal("failed to look up merchant").Wrap(err)
	}
	if m != nil {
		return m, nil
	}
	m, err = s.merchants.Create(ctx, name)
	if err != nil {
		return nil, apperror.Internal("failed to create merchant").Wrap(err)
	}
	if m == nil {
		return nil, apperror.Internal("failed to create merchant")
	}
	return m, nil
}

// Get loads one of the caller's merchants.
func (s *MerchantService) Get(ctx context.Context, id uuid.UUID) (*entity.Merchant, error) {
	m, err := s.merchants.Get(ctx, id)
	if err != nil {
		return nil, apperror.Internal("failed to load merchant").Wrap(err)
	}
	if m == nil {
		return nil, apperror.NotFound("merchant not found")
	}
	return m, nil
}

// List is the Settings → Merchants table.
func (s *MerchantService) List(ctx context.Context) ([]repository.MerchantRow, error) {
	rows, err := s.merchants.List(ctx)
	if err != nil {
		return nil, apperror.Internal("failed to list merchants").Wrap(err)
	}
	return rows, nil
}

// Rename renames a merchant and rewrites the display text on its rows.
// Renaming onto an existing name is a merge, and says so.
func (s *MerchantService) Rename(ctx context.Context, id uuid.UUID, name string) (*entity.Merchant, error) {
	clean, err := validMerchantName(name)
	if err != nil {
		return nil, err
	}
	m, err := s.merchants.Rename(ctx, id, clean)
	if err != nil {
		if errors.Is(err, repository.ErrMerchantNameTaken) {
			return nil, apperror.Conflict("merchant name already exists")
		}
		return nil, apperror.Internal("failed to rename merchant").Wrap(err)
	}
	if m == nil {
		return nil, apperror.NotFound("merchant not found")
	}
	if err := s.merchants.PutAlias(ctx, repository.AliasKey(clean), m.ID, "user"); err != nil {
		return nil, apperror.Internal("failed to remember merchant").Wrap(err)
	}
	return m, nil
}

// Merge folds one merchant into another; the source disappears and its
// rows, aliases and old name follow the survivor.
func (s *MerchantService) Merge(ctx context.Context, fromID, intoID uuid.UUID) error {
	if fromID == intoID {
		return apperror.Validation("invalid merge").
			WithDetails(apperror.FieldError{Field: "into_merchant_id", Rule: "distinct", Message: "cannot merge a merchant into itself"})
	}
	if err := s.merchants.Merge(ctx, fromID, intoID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apperror.NotFound("merchant not found")
		}
		return apperror.Internal("failed to merge merchants").Wrap(err)
	}
	return nil
}

func validMerchantName(name string) (string, error) {
	clean := strings.Join(strings.Fields(name), " ")
	if clean == "" || len([]rune(clean)) > merchantNameMaxLen {
		return "", apperror.Validation("invalid merchant name").
			WithDetails(apperror.FieldError{Field: "name", Rule: "length", Message: "name must be 1 to 80 characters"})
	}
	return clean, nil
}

// titleCase is the offline tidy-up: one capital per word. An all-caps
// token of three letters or fewer ("BCA", "KFC") is left as typed —
// lower-casing an acronym reads as a typo.
func titleCase(s string) string {
	words := strings.Fields(s)
	for i, w := range words {
		runes := []rune(w)
		if len(runes) <= 3 && isAllUpper(runes) {
			continue
		}
		for j, r := range runes {
			if j == 0 {
				runes[j] = unicode.ToUpper(r)
			} else {
				runes[j] = unicode.ToLower(r)
			}
		}
		words[i] = string(runes)
	}
	return strings.Join(words, " ")
}

func isAllUpper(runes []rune) bool {
	letters := 0
	for _, r := range runes {
		if !unicode.IsLetter(r) {
			return false
		}
		if !unicode.IsUpper(r) {
			return false
		}
		letters++
	}
	return letters > 0
}
