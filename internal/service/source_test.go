package service_test

import (
	"context"
	"testing"

	"github.com/glennprays/catet-backend/internal/apperror"
	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/glennprays/catet-backend/internal/service"
	"github.com/glennprays/catet-backend/internal/testkit"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sourceFixture exercises the full Source lifecycle over a live
// Postgres: balances come from legs, so archiving and deleting can
// only be judged against real ledger rows.
type sourceFixture struct {
	ctx      context.Context
	pool     *pgxpool.Pool
	sources  *service.SourceService
	repo     *repository.SourceRepository
	settings *repository.SettingsRepository
	bills    *service.BillService
}

func newSourceFixture(t *testing.T) *sourceFixture {
	t.Helper()
	pool := testkit.NewDB(t)
	ctx, _ := testkit.Scoped(t, pool, "+628111000041", "Sources")

	sourceRepo := repository.NewSourceRepository(pool)
	billRepo := repository.NewBillRepository(pool)
	paymentRepo := repository.NewBillPaymentRepository(pool)
	txSvc := service.NewTransactionService(
		repository.NewTransactionRepository(pool), sourceRepo,
		repository.NewCategoryRepository(pool), paymentRepo, billRepo, testkit.Logger(t),
	)

	return &sourceFixture{
		ctx:      ctx,
		pool:     pool,
		sources:  service.NewSourceService(sourceRepo, txSvc),
		repo:     sourceRepo,
		settings: repository.NewSettingsRepository(pool),
		bills:    service.NewBillService(billRepo, paymentRepo, txSvc, sourceRepo),
	}
}

func (f *sourceFixture) create(t *testing.T, name, typ string) *entity.Source {
	t.Helper()
	out, err := f.sources.Create(f.ctx, service.CreateSourceInput{Name: name, Type: typ})
	require.NoError(t, err)
	return &out.Source
}

func TestArchiveSourceRejectsNonZeroBalance(t *testing.T) {
	f := newSourceFixture(t)
	src := f.create(t, "Dompet", "cash")

	_, err := f.sources.Adjust(f.ctx, src.ID, 100_000, nil)
	require.NoError(t, err)

	err = f.sources.Archive(f.ctx, src.ID)
	require.Error(t, err)
	assert.ErrorIs(t, err, apperror.ErrConflict)
	assert.Contains(t, err.Error(), "still holds a balance")

	still, err := f.repo.Get(f.ctx, src.ID)
	require.NoError(t, err)
	require.NotNil(t, still)
	assert.Nil(t, still.ArchivedAt, "source must stay active when the archive is refused")
}

func TestArchiveSourceAtZeroClearsPhotoDefault(t *testing.T) {
	f := newSourceFixture(t)
	src := f.create(t, "Dompet", "cash")

	_, err := f.sources.Adjust(f.ctx, src.ID, 100_000, nil)
	require.NoError(t, err)
	require.NoError(t, f.settings.SetPhotoDefaultSource(f.ctx, &src.ID))

	// Empty it with a correcting Adjustment, then archiving is allowed.
	_, err = f.sources.Adjust(f.ctx, src.ID, -100_000, nil)
	require.NoError(t, err)
	require.NoError(t, f.sources.Archive(f.ctx, src.ID))

	archived, err := f.repo.Get(f.ctx, src.ID)
	require.NoError(t, err)
	require.NotNil(t, archived)
	assert.NotNil(t, archived.ArchivedAt)

	def, err := f.settings.GetPhotoDefaultSource(f.ctx)
	require.NoError(t, err)
	assert.Nil(t, def, "archived source must not stay the receipt fallback")
}

func TestRestoreSource(t *testing.T) {
	f := newSourceFixture(t)
	src := f.create(t, "Dompet", "cash")
	require.NoError(t, f.sources.Archive(f.ctx, src.ID))

	out, err := f.sources.Restore(f.ctx, src.ID)
	require.NoError(t, err)
	assert.Nil(t, out.ArchivedAt)

	_, err = f.sources.Restore(f.ctx, src.ID)
	require.Error(t, err)
	assert.ErrorIs(t, err, apperror.ErrConflict)

	_, err = f.sources.Restore(f.ctx, uuid.New())
	assert.ErrorIs(t, err, apperror.ErrNotFound)
}

func TestDeletePermanentlyRejectsSourceWithHistory(t *testing.T) {
	f := newSourceFixture(t)
	src := f.create(t, "Dompet", "cash")
	_, err := f.sources.Adjust(f.ctx, src.ID, 50_000, nil)
	require.NoError(t, err)

	err = f.sources.DeletePermanently(f.ctx, src.ID)
	require.Error(t, err)
	assert.ErrorIs(t, err, apperror.ErrConflict)

	still, err := f.repo.Get(f.ctx, src.ID)
	require.NoError(t, err)
	assert.NotNil(t, still)
}

func TestDeletePermanentlyUnlinksBills(t *testing.T) {
	f := newSourceFixture(t)
	src := f.create(t, "Dompet", "cash")

	bill, err := f.bills.Create(f.ctx, service.CreateBillInput{
		Name: "Listrik", AmountMinMinor: 200_000, AmountMaxMinor: 250_000,
		Cadence: "monthly", NextDueDate: dateStr(service.JakartaToday()),
		LinkedSourceID: &src.ID,
	})
	require.NoError(t, err)

	require.NoError(t, f.sources.DeletePermanently(f.ctx, src.ID))

	gone, err := f.repo.Get(f.ctx, src.ID)
	require.NoError(t, err)
	assert.Nil(t, gone)

	var linked *uuid.UUID
	require.NoError(t, f.pool.QueryRow(f.ctx,
		`SELECT linked_source_id FROM bills WHERE id = $1`, bill.ID,
	).Scan(&linked))
	assert.Nil(t, linked, "bill keeps existing; the source link is only a hint")
}

func TestUpdateSourceChangesTypeAndReportsHistory(t *testing.T) {
	f := newSourceFixture(t)
	src := f.create(t, "BCA", "bank")

	ewallet := "ewallet"
	out, err := f.sources.Update(f.ctx, src.ID, service.UpdateSourceInput{Type: &ewallet})
	require.NoError(t, err)
	assert.Equal(t, "ewallet", out.Type)
	assert.Equal(t, "BCA", out.Name, "an untouched field stays put")
	assert.False(t, out.HasTransactions)

	_, err = f.sources.Adjust(f.ctx, src.ID, 25_000, nil)
	require.NoError(t, err)

	listed, err := f.sources.List(f.ctx, false)
	require.NoError(t, err)
	require.Len(t, listed.Sources, 1)
	assert.True(t, listed.Sources[0].HasTransactions)
	assert.EqualValues(t, 25_000, listed.Sources[0].BalanceMinor)
}
