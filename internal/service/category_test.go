package service_test

import (
	"context"
	"errors"
	"testing"

	"github.com/glennprays/catet-backend/internal/apperror"
	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/glennprays/catet-backend/internal/service"
	"github.com/glennprays/catet-backend/internal/testkit"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// categoryFixture exercises the category tree over a live Postgres: the
// archive cascade is a recursive CTE, so nothing below can be judged
// against a fake.
type categoryFixture struct {
	ctx     context.Context
	cats    *service.CategoryService
	budgets *service.BudgetService
}

func newCategoryFixture(t *testing.T) *categoryFixture {
	t.Helper()
	pool := testkit.NewDB(t)
	ctx, _ := testkit.Scoped(t, pool, "+628111000051", "Categories")

	categoryRepo := repository.NewCategoryRepository(pool)
	analyticsSvc := service.NewAnalyticsService(
		repository.NewAnalyticsRepository(pool), categoryRepo,
		repository.NewSourceRepository(pool), repository.NewBillRepository(pool),
		repository.NewSettingsRepository(pool),
	)

	return &categoryFixture{
		ctx:     ctx,
		cats:    service.NewCategoryService(categoryRepo),
		budgets: service.NewBudgetService(repository.NewBudgetRepository(pool), categoryRepo, analyticsSvc),
	}
}

func (f *categoryFixture) create(t *testing.T, name string, parent *uuid.UUID) *entity.Category {
	t.Helper()
	cat, err := f.cats.Create(f.ctx, service.CreateCategoryInput{Name: name, ParentID: parent})
	require.NoError(t, err)
	return cat
}

func (f *categoryFixture) byName(t *testing.T, includeArchived bool) map[string]entity.Category {
	t.Helper()
	list, err := f.cats.List(f.ctx, includeArchived)
	require.NoError(t, err)
	out := make(map[string]entity.Category, len(list))
	for _, c := range list {
		out[c.Name] = c
	}
	return out
}

// TestArchiveCascadesToSubtree is the load-bearing case: an archived
// parent with live children is a tree the dashboard cannot draw, so the
// whole subtree goes at once — at any depth.
func TestArchiveCascadesToSubtree(t *testing.T) {
	f := newCategoryFixture(t)

	tagihan := f.create(t, "Tagihan", nil)
	listrik := f.create(t, "Listrik", &tagihan.ID)
	token := f.create(t, "Token", &listrik.ID)
	f.create(t, "Makan", nil)

	require.NoError(t, f.cats.Archive(f.ctx, tagihan.ID))

	live := f.byName(t, false)
	require.Len(t, live, 1, "only the untouched sibling root stays live")
	assert.Contains(t, live, "Makan")

	all := f.byName(t, true)
	for _, name := range []string{"Tagihan", "Listrik", "Token"} {
		row, ok := all[name]
		require.True(t, ok, "%s must still exist as history", name)
		assert.NotNil(t, row.ArchivedAt, "%s must be archived with its parent", name)
	}
	assert.Nil(t, all["Makan"].ArchivedAt)

	// Idempotent: archiving again is a no-op, not a 404 or an error.
	assert.NoError(t, f.cats.Archive(f.ctx, tagihan.ID))
	assert.NotNil(t, token.ID)
}

// TestArchiveParentBlockedByChildBudget: the current-period budget guard
// covers the subtree, so a parent cannot be archived out from under a
// child the user is still budgeting.
func TestArchiveParentBlockedByChildBudget(t *testing.T) {
	f := newCategoryFixture(t)

	transport := f.create(t, "Transport", nil)
	bensin := f.create(t, "Bensin", &transport.ID)

	_, err := f.budgets.Create(f.ctx, bensin.ID, service.JakartaMonthPeriod(service.JakartaNow()), 500_000_00)
	require.NoError(t, err)

	err = f.cats.Archive(f.ctx, transport.ID)
	require.Error(t, err)
	var ae *apperror.Error
	require.True(t, errors.As(err, &ae))
	assert.Equal(t, apperror.KindConflict, ae.Kind)

	live := f.byName(t, false)
	assert.Contains(t, live, "Transport", "a refused archive changes nothing")
	assert.Contains(t, live, "Bensin")
}

// TestArchivedParentRefusesChildren: an archived category is a dead end.
// It takes no new child, no re-parent, and no edit.
func TestArchivedParentRefusesChildren(t *testing.T) {
	f := newCategoryFixture(t)

	lama := f.create(t, "Lama", nil)
	live := f.create(t, "Aktif", nil)
	require.NoError(t, f.cats.Archive(f.ctx, lama.ID))

	assertArchivedField := func(t *testing.T, err error, field string) {
		t.Helper()
		require.Error(t, err)
		var ae *apperror.Error
		require.True(t, errors.As(err, &ae))
		assert.Equal(t, apperror.KindValidation, ae.Kind)
		require.Len(t, ae.Details, 1)
		assert.Equal(t, field, ae.Details[0].Field)
		assert.Equal(t, "archived", ae.Details[0].Rule)
	}

	_, err := f.cats.Create(f.ctx, service.CreateCategoryInput{Name: "Baru", ParentID: &lama.ID})
	assertArchivedField(t, err, "parent_id")

	newParent := &lama.ID
	_, err = f.cats.Update(f.ctx, live.ID, service.UpdateCategoryInput{ParentID: &newParent})
	assertArchivedField(t, err, "parent_id")

	name := "Lama Sekali"
	_, err = f.cats.Update(f.ctx, lama.ID, service.UpdateCategoryInput{Name: &name})
	require.Error(t, err)
	var ae *apperror.Error
	require.True(t, errors.As(err, &ae))
	assert.Equal(t, apperror.KindConflict, ae.Kind, "an archived category is history, not an editable row")

	// A missing parent is a field error, not an FK 500.
	missing := uuid.New()
	missingPtr := &missing
	_, err = f.cats.Update(f.ctx, live.ID, service.UpdateCategoryInput{ParentID: &missingPtr})
	require.Error(t, err)
	require.True(t, errors.As(err, &ae))
	assert.Equal(t, apperror.KindValidation, ae.Kind)
	require.Len(t, ae.Details, 1)
	assert.Equal(t, "exists", ae.Details[0].Rule)
}

// TestArchivedCategoryRefusedOnNewBudget: an existing budget row on an
// archived category is history and keeps rendering, but a new one is a
// user choosing a category that is gone.
func TestArchivedCategoryRefusedOnNewBudget(t *testing.T) {
	f := newCategoryFixture(t)

	cat := f.create(t, "Hiburan", nil)
	require.NoError(t, f.cats.Archive(f.ctx, cat.ID))

	_, err := f.budgets.Create(f.ctx, cat.ID, service.JakartaMonthPeriod(service.JakartaNow()), 100_000_00)
	require.Error(t, err)
	var ae *apperror.Error
	require.True(t, errors.As(err, &ae))
	assert.Equal(t, apperror.KindValidation, ae.Kind)
	require.Len(t, ae.Details, 1)
	assert.Equal(t, "category_id", ae.Details[0].Field)
	assert.Equal(t, "archived", ae.Details[0].Rule)
}
