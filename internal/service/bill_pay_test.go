package service_test

import (
	"context"
	"testing"
	"time"

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

// billFixture wires the paid loop over a live Postgres: paying a bill
// writes a real expense through the same ledger engine the bot uses.
type billFixture struct {
	ctx      context.Context
	pool     *pgxpool.Pool
	bills    *service.BillService
	billRepo *repository.BillRepository
	payments *repository.BillPaymentRepository
	txs      *service.TransactionService
	cash     *entity.Source
}

func newBillFixture(t *testing.T) *billFixture {
	t.Helper()
	pool := testkit.NewDB(t)
	ctx, _ := testkit.Scoped(t, pool, "+628111000021", "Bills")

	sourceRepo := repository.NewSourceRepository(pool)
	billRepo := repository.NewBillRepository(pool)
	paymentRepo := repository.NewBillPaymentRepository(pool)
	txSvc := service.NewTransactionService(
		repository.NewTransactionRepository(pool), sourceRepo,
		repository.NewCategoryRepository(pool), paymentRepo, billRepo, testkit.Logger(t),
	)
	cash, err := sourceRepo.Create(ctx, &entity.Source{
		ID: uuid.New(), Name: "Cash", Type: "cash", Currency: "IDR",
	})
	require.NoError(t, err)

	return &billFixture{
		ctx: ctx, pool: pool,
		bills:    service.NewBillService(billRepo, paymentRepo, txSvc, sourceRepo),
		billRepo: billRepo, payments: paymentRepo, txs: txSvc, cash: cash,
	}
}

func (f *billFixture) create(t *testing.T, in service.CreateBillInput) *entity.Bill {
	t.Helper()
	if in.Name == "" {
		in.Name = "Listrik"
	}
	if in.AmountMinMinor == 0 {
		in.AmountMinMinor = 200_000
	}
	if in.AmountMaxMinor == 0 {
		in.AmountMaxMinor = 250_000
	}
	if in.LinkedSourceID == nil {
		in.LinkedSourceID = &f.cash.ID
	}
	bill, err := f.bills.Create(f.ctx, in)
	require.NoError(t, err)
	return bill
}

func dateStr(d time.Time) string { return d.Format("2006-01-02") }

func TestBillPayAdvancesTheSchedule(t *testing.T) {
	f := newBillFixture(t)
	today := service.JakartaToday()

	bill := f.create(t, service.CreateBillInput{
		Cadence: "monthly", NextDueDate: dateStr(today),
	})

	out, err := f.bills.Pay(f.ctx, bill.ID, service.PayBillInput{
		AmountMinor: 230_000, Origin: "dashboard_manual",
	})
	require.NoError(t, err)

	assert.Equal(t, "expense", out.Transaction.Type)
	assert.EqualValues(t, 230_000, out.Transaction.AmountMinor)
	require.NotNil(t, out.Transaction.Merchant)
	assert.Equal(t, "Listrik", *out.Transaction.Merchant)

	wantNext, ok := service.NextOccurrence("monthly", today, today.Day())
	require.True(t, ok)
	assert.True(t, out.Bill.NextDueDate.Equal(wantNext),
		"paid bill must move to %s, got %s", dateStr(wantNext), dateStr(out.Bill.NextDueDate))
	assert.True(t, out.Bill.Active)
	assert.Nil(t, out.Bill.CompletedAt)

	// The link row is what lets a void rewind the schedule.
	p, err := f.payments.ByTransaction(f.ctx, out.Transaction.ID)
	require.NoError(t, err)
	require.NotNil(t, p)
	assert.True(t, p.DueDate.Equal(today))
	require.NotNil(t, p.AdvancedTo)
	assert.True(t, p.AdvancedTo.Equal(wantNext))

	// And the detail read says which bill it settled.
	detail, err := f.txs.Get(f.ctx, out.Transaction.ID)
	require.NoError(t, err)
	require.NotNil(t, detail.BillID)
	assert.Equal(t, bill.ID, *detail.BillID)
	require.NotNil(t, detail.BillName)
	assert.Equal(t, "Listrik", *detail.BillName)
}

func TestBillPayCompletesOneTimeAndTenor(t *testing.T) {
	today := service.JakartaToday()

	t.Run("once completes on the first payment", func(t *testing.T) {
		f := newBillFixture(t)
		bill := f.create(t, service.CreateBillInput{
			Name: "Deposit", Cadence: "once", NextDueDate: dateStr(today),
		})
		out, err := f.bills.Pay(f.ctx, bill.ID, service.PayBillInput{
			AmountMinor: 200_000, Origin: "dashboard_manual",
		})
		require.NoError(t, err)
		assert.False(t, out.Bill.Active, "a one-time bill has nothing left to be due")
		require.NotNil(t, out.Bill.CompletedAt)

		_, err = f.bills.Pay(f.ctx, bill.ID, service.PayBillInput{
			AmountMinor: 200_000, Origin: "dashboard_manual",
		})
		require.Error(t, err, "a completed bill cannot be paid again")
		var appErr *apperror.Error
		require.ErrorAs(t, err, &appErr)
		assert.Equal(t, apperror.KindConflict, appErr.Kind)
	})

	t.Run("tenor completes on the last installment", func(t *testing.T) {
		f := newBillFixture(t)
		bill := f.create(t, service.CreateBillInput{
			Name: "Cicilan", Cadence: "monthly", NextDueDate: dateStr(today),
			RemainingCount: new(2),
		})
		out, err := f.bills.Pay(f.ctx, bill.ID, service.PayBillInput{
			AmountMinor: 100_000, Origin: "dashboard_manual",
		})
		require.NoError(t, err)
		require.NotNil(t, out.Bill.RemainingCount)
		assert.Equal(t, 1, *out.Bill.RemainingCount)
		assert.True(t, out.Bill.Active)

		out, err = f.bills.Pay(f.ctx, bill.ID, service.PayBillInput{
			AmountMinor: 100_000, Origin: "dashboard_manual",
		})
		require.NoError(t, err)
		assert.False(t, out.Bill.Active)
		require.NotNil(t, out.Bill.CompletedAt)
		require.NotNil(t, out.Bill.RemainingCount)
		assert.Equal(t, 0, *out.Bill.RemainingCount,
			"the final installment is consumed, not left hanging at 1")
	})

	t.Run("a tenor on a one-time bill is refused", func(t *testing.T) {
		f := newBillFixture(t)
		_, err := f.bills.Create(f.ctx, service.CreateBillInput{
			Name: "Deposit", AmountMinMinor: 1, AmountMaxMinor: 1,
			Cadence: "once", NextDueDate: dateStr(today), RemainingCount: new(3),
		})
		var appErr *apperror.Error
		require.ErrorAs(t, err, &appErr)
		assert.Equal(t, apperror.KindValidation, appErr.Kind)
		require.Len(t, appErr.Details, 1)
		assert.Equal(t, "remaining_count", appErr.Details[0].Field)
	})
}

func TestBillPayNeedsASource(t *testing.T) {
	f := newBillFixture(t)
	today := service.JakartaToday()

	bill, err := f.bills.Create(f.ctx, service.CreateBillInput{
		Name: "Kos", AmountMinMinor: 1_000_000, AmountMaxMinor: 1_000_000,
		Cadence: "monthly", NextDueDate: dateStr(today),
	})
	require.NoError(t, err)

	_, err = f.bills.Pay(f.ctx, bill.ID, service.PayBillInput{
		AmountMinor: 1_000_000, Origin: "dashboard_manual",
	})
	var appErr *apperror.Error
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, apperror.KindValidation, appErr.Kind)
	require.Len(t, appErr.Details, 1)
	assert.Equal(t, "source_id", appErr.Details[0].Field)

	// Naming one at payment time is enough; the bill keeps no source.
	out, err := f.bills.Pay(f.ctx, bill.ID, service.PayBillInput{
		AmountMinor: 1_000_000, SourceID: &f.cash.ID, Origin: "dashboard_manual",
	})
	require.NoError(t, err)
	assert.Nil(t, out.Bill.LinkedSourceID)
}

func TestBillPayVoidRewindsTheBill(t *testing.T) {
	today := service.JakartaToday()

	t.Run("void restores the due date and drops the link", func(t *testing.T) {
		f := newBillFixture(t)
		bill := f.create(t, service.CreateBillInput{
			Cadence: "monthly", NextDueDate: dateStr(today), RemainingCount: new(3),
		})
		out, err := f.bills.Pay(f.ctx, bill.ID, service.PayBillInput{
			AmountMinor: 230_000, Origin: "dashboard_manual",
		})
		require.NoError(t, err)

		require.NoError(t, f.txs.Void(f.ctx, out.Transaction.ID))

		after, err := f.billRepo.Get(f.ctx, bill.ID)
		require.NoError(t, err)
		assert.True(t, after.NextDueDate.Equal(today), "the occurrence is unpaid again")
		require.NotNil(t, after.RemainingCount)
		assert.Equal(t, 3, *after.RemainingCount, "the installment is given back")

		p, err := f.payments.ByTransaction(f.ctx, out.Transaction.ID)
		require.NoError(t, err)
		assert.Nil(t, p, "the link row goes with the payment")
	})

	t.Run("void of a completing payment reopens the bill", func(t *testing.T) {
		f := newBillFixture(t)
		bill := f.create(t, service.CreateBillInput{
			Name: "Deposit", Cadence: "once", NextDueDate: dateStr(today),
		})
		out, err := f.bills.Pay(f.ctx, bill.ID, service.PayBillInput{
			AmountMinor: 200_000, Origin: "dashboard_manual",
		})
		require.NoError(t, err)
		require.NoError(t, f.txs.Void(f.ctx, out.Transaction.ID))

		after, err := f.billRepo.Get(f.ctx, bill.ID)
		require.NoError(t, err)
		assert.True(t, after.Active)
		assert.Nil(t, after.CompletedAt)
		assert.True(t, after.NextDueDate.Equal(today))
	})

	t.Run("a bill rescheduled after payment wins over the undo", func(t *testing.T) {
		f := newBillFixture(t)
		bill := f.create(t, service.CreateBillInput{
			Cadence: "monthly", NextDueDate: dateStr(today),
		})
		out, err := f.bills.Pay(f.ctx, bill.ID, service.PayBillInput{
			AmountMinor: 230_000, Origin: "dashboard_manual",
		})
		require.NoError(t, err)

		edited := dateStr(today.AddDate(0, 0, 10))
		_, err = f.bills.Update(f.ctx, bill.ID, service.UpdateBillInput{NextDueDate: &edited})
		require.NoError(t, err)

		require.NoError(t, f.txs.Void(f.ctx, out.Transaction.ID), "the void itself never fails on the bill")

		after, err := f.billRepo.Get(f.ctx, bill.ID)
		require.NoError(t, err)
		assert.Equal(t, edited, dateStr(after.NextDueDate), "the newer manual edit stands")

		p, err := f.payments.ByTransaction(f.ctx, out.Transaction.ID)
		require.NoError(t, err)
		assert.Nil(t, p)
	})
}

func TestBillReactivate(t *testing.T) {
	f := newBillFixture(t)
	today := service.JakartaToday()

	bill := f.create(t, service.CreateBillInput{Cadence: "monthly", NextDueDate: dateStr(today)})
	require.NoError(t, f.bills.Deactivate(f.ctx, bill.ID))
	require.NoError(t, f.bills.Reactivate(f.ctx, bill.ID))

	after, err := f.billRepo.Get(f.ctx, bill.ID)
	require.NoError(t, err)
	assert.True(t, after.Active)

	// A completed bill is spent, not paused.
	once := f.create(t, service.CreateBillInput{Name: "Deposit", Cadence: "once", NextDueDate: dateStr(today)})
	_, err = f.bills.Pay(f.ctx, once.ID, service.PayBillInput{AmountMinor: 200_000, Origin: "dashboard_manual"})
	require.NoError(t, err)

	err = f.bills.Reactivate(f.ctx, once.ID)
	var appErr *apperror.Error
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, apperror.KindConflict, appErr.Kind)
}

func TestBillPaymentsFeedTheCalendar(t *testing.T) {
	f := newBillFixture(t)
	today := service.JakartaToday()

	bill := f.create(t, service.CreateBillInput{Cadence: "monthly", NextDueDate: dateStr(today)})
	_, err := f.bills.Pay(f.ctx, bill.ID, service.PayBillInput{AmountMinor: 230_000, Origin: "dashboard_manual"})
	require.NoError(t, err)

	got, err := f.bills.Payments(f.ctx, dateStr(today), dateStr(today.AddDate(0, 0, 1)))
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "Listrik", got[0].BillName)
	assert.EqualValues(t, 230_000, got[0].AmountMinor)

	empty, err := f.bills.Payments(f.ctx, dateStr(today.AddDate(0, 0, 5)), dateStr(today.AddDate(0, 0, 6)))
	require.NoError(t, err)
	assert.Empty(t, empty)
	assert.NotNil(t, empty, "an empty range is [], never null")
}
