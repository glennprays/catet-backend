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

	out := &response.SourcesData{Sources: []response.SourceResponse{}}
	for _, src := range sources {
		balance := balances[src.ID]
		out.Sources = append(out.Sources, response.SourceResponse{Source: src, BalanceMinor: balance})
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
	updated, err := s.repo.Update(ctx, id, in.Name, in.IsDebt)
	if err != nil {
		return nil, apperror.Internal("failed to update source").Wrap(err)
	}
	balances, err := s.repo.Balances(ctx)
	if err != nil {
		return nil, apperror.Internal("failed to compute balances").Wrap(err)
	}
	return &response.SourceResponse{Source: *updated, BalanceMinor: balances[id]}, nil
}

// Archive soft-deletes a source; blocked while open (active) bills
// reference it.
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
	if _, err := s.repo.Archive(ctx, id); err != nil {
		return apperror.Internal("failed to archive source").Wrap(err)
	}
	return nil
}

// Adjust writes a correction journal on the source (flows.md §8).
func (s *SourceService) Adjust(ctx context.Context, id uuid.UUID, amountMinor int64, note *string) (*entity.Transaction, error) {
	return s.transaction.Adjust(ctx, id, amountMinor, note)
}
