package service

import (
	"context"
	"fmt"

	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/google/uuid"
)

// OnboardingService seeds a brand-new account so its first screen is
// usable: an empty Categories page and an empty Sources rail would make
// the first capture impossible.
type OnboardingService struct {
	cats    *repository.CategoryRepository
	sources *repository.SourceRepository
}

func NewOnboardingService(cats *repository.CategoryRepository, sources *repository.SourceRepository) *OnboardingService {
	return &OnboardingService{cats: cats, sources: sources}
}

// SeedDefaults writes the default category tree and the Cash Source
// under the account the context is scoped to. It is not idempotent:
// callers run it exactly once, right after the user row exists.
func (s *OnboardingService) SeedDefaults(ctx context.Context) error {
	for _, def := range DefaultCategories {
		parent, err := s.cats.Create(ctx, &entity.Category{
			ID: uuid.New(), Name: def.Name, Essential: def.Essential,
		})
		if err != nil {
			return fmt.Errorf("seed category %s: %w", def.Name, err)
		}
		for _, child := range def.Children {
			if _, err := s.cats.Create(ctx, &entity.Category{
				ID: uuid.New(), Name: child, ParentID: &parent.ID, Essential: true,
			}); err != nil {
				return fmt.Errorf("seed category %s: %w", child, err)
			}
		}
	}
	if _, err := s.sources.Create(ctx, &entity.Source{
		ID: uuid.New(), Name: DefaultSourceName, Type: "cash", Currency: "IDR",
	}); err != nil {
		return fmt.Errorf("seed source %s: %w", DefaultSourceName, err)
	}
	return nil
}
