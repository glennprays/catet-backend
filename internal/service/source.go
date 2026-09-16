package service

import (
	"context"

	"github.com/glennprays/catet-backend/internal/apperror"
	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/params/response"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/google/uuid"
)

// SourceService handles source CRUD and the balance rollup the rail
// renders. Balances are ledger-computed, never stored.
type SourceService struct {
	repo        *repository.SourceRepository
	transaction *TransactionService
}

func NewSourceService(repo *repository.SourceRepository, transaction *TransactionService) *SourceService {
	return &SourceService{repo: repo, transaction: transaction}
}

// List returns sources with balances plus ready-to-render totals:
// total available (non-debt) and total debt (positive magnitude of
// negative debt balances).
func (s *SourceService) List(ctx context.Context, includeArchived bool) (*response.SourcesData, error) {
	sources, err := s.repo.List(ctx, includeArchived)
	if err != nil {
		return nil, apperror.Internal("failed to list sources").Wrap(err)
	}
	balances, err := s.repo.Balances(ctx)
	if err != nil {
		return nil, apperror.Internal("failed to compute balances").Wrap(err)
	}
	history, err := s.repo.WithHistory(ctx)
	if err != nil {
		return nil, apperror.Internal("failed to load source history").Wrap(err)
	}

	out := &response.SourcesData{Sources: []response.SourceResponse{}}
	for _, src := range sources {
		balance := balances[src.ID]
		out.Sources = append(out.Sources, response.SourceResponse{
			Source:          src,
			BalanceMinor:    balance,
			HasTransactions: history[src.ID],
		})
		if src.IsDebt {
			if balance < 0 {
				out.TotalDebtMinor += -balance
			}
		} else {
			out.TotalAvailableMinor += balance
		}
	}
	return out, nil
}

// CreateInput is the parsed POST /sources body.
type CreateSourceInput struct {
	Name     string
	Type     string
	IsDebt   bool
	Currency string
}

func (s *SourceService) Create(ctx context.Context, in CreateSourceInput) (*response.SourceResponse, error) {
	currency := in.Currency
	if currency == "" {
		currency = "IDR"
	}
	created, err := s.repo.Create(ctx, &entity.Source{
		ID:       uuid.New(),
		Name:     in.Name,
		Type:     in.Type,
		IsDebt:   in.IsDebt,
		Currency: currency,
	})
	if err != nil {
		return nil, apperror.Internal("failed to create source").Wrap(err)
	}
	// A fresh source has no legs yet.
	return &response.SourceResponse{Source: *created, BalanceMinor: 0}, nil
}

// UpdateInput is the parsed PATCH /sources/{id} body.
type UpdateSourceInput struct {
	Name   *string
	Type   *string
	IsDebt *bool
}

func (s *SourceService) Update(ctx context.Context, id uuid.UUID, in UpdateSourceInput) (*response.SourceResponse, error) {
	existing, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, apperror.Internal("failed to load source").Wrap(err)
	}
	if existing == nil {
		return nil, apperror.NotFound("source not found")
	}
	updated, err := s.repo.Update(ctx, id, in.Name, in.Type, in.IsDebt)
	if err != nil {
		return nil, apperror.Internal("failed to update source").Wrap(err)
	}
	return s.decorate(ctx, updated)
}

// Restore lifts the archive flag. Flexible by design: an archived
// source is never gone, only out of the way.
func (s *SourceService) Restore(ctx context.Context, id uuid.UUID) (*response.SourceResponse, error) {
	existing, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, apperror.Internal("failed to load source").Wrap(err)
	}
	if existing == nil {
		return nil, apperror.NotFound("source not found")
	}
	if existing.ArchivedAt == nil {
		return nil, apperror.Conflict("source is not archived")
	}
	restored, err := s.repo.Unarchive(ctx, id)
	if err != nil {
		return nil, apperror.Internal("failed to restore source").Wrap(err)
	}
	if restored == nil {
		return nil, apperror.NotFound("source not found")
	}
	return s.decorate(ctx, restored)
}

// DeletePermanently removes a source that never carried a transaction.
// Anything with history must be archived instead — the ledger is
// append-only and its legs must keep pointing somewhere.
func (s *SourceService) DeletePermanently(ctx context.Context, id uuid.UUID) error {
	existing, err := s.repo.Get(ctx, id)
	if err != nil {
		return apperror.Internal("failed to load source").Wrap(err)
	}
	if existing == nil {
		return apperror.NotFound("source not found")
	}
	history, err := s.repo.WithHistory(ctx)
	if err != nil {
		return apperror.Internal("failed to load source history").Wrap(err)
	}
	if history[id] {
		return apperror.Conflict("source has transactions; archive it instead")
	}
	deleted, err := s.repo.DeletePermanently(ctx, id)
	if err != nil {
		return apperror.Internal("failed to delete source").Wrap(err)
	}
	if !deleted {
		return apperror.NotFound("source not found")
	}
	return nil
}

// decorate attaches the balance and history flag every source
// response carries.
func (s *SourceService) decorate(ctx context.Context, src *entity.Source) (*response.SourceResponse, error) {
	balances, err := s.repo.Balances(ctx)
	if err != nil {
		return nil, apperror.Internal("failed to compute balances").Wrap(err)
	}
	history, err := s.repo.WithHistory(ctx)
	if err != nil {
		return nil, apperror.Internal("failed to load source history").Wrap(err)
	}
	return &response.SourceResponse{
		Source:          *src,
		BalanceMinor:    balances[src.ID],
		HasTransactions: history[src.ID],
	}, nil
}

// Archive soft-deletes a source; blocked while open (active) bills
// reference it or while it still holds money (empty it with a
// transfer or an Adjustment first).
func (s *SourceService) Archive(ctx context.Context, id uuid.UUID) error {
	existing, err := s.repo.Get(ctx, id)
	if err != nil {
		return apperror.Internal("failed to load source").Wrap(err)
	}
	if existing == nil {
		return apperror.NotFound("source not found")
	}
	hasBills, err := s.repo.HasActiveBills(ctx, id)
	if err != nil {
		return apperror.Internal("failed to check bills").Wrap(err)
	}
	if hasBills {
		return apperror.Conflict("source is referenced by active bills")
	}
	balances, err := s.repo.Balances(ctx)
	if err != nil {
		return apperror.Internal("failed to compute balances").Wrap(err)
	}
	if balances[id] != 0 {
		return apperror.Conflict("source still holds a balance")
	}
	// An archived source must not stay the bot's photo default.
	if err := s.repo.ClearAsPhotoDefault(ctx, id); err != nil {
		return apperror.Internal("failed to clear photo default").Wrap(err)
	}
	if _, err := s.repo.Archive(ctx, id); err != nil {
		return apperror.Internal("failed to archive source").Wrap(err)
	}
	return nil
}

// Adjust writes a correction journal on the source (flows.md §8).
func (s *SourceService) Adjust(ctx context.Context, id uuid.UUID, amountMinor int64, note *string) (*entity.Transaction, error) {
	return s.transaction.Adjust(ctx, id, amountMinor, note)
}
