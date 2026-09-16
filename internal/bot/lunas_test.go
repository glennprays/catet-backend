package bot_test

import (
	"testing"
	"time"

	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bill seeds an active bill for the fixture's owner.
func (f *botFixture) bill(t *testing.T, name string, min, max int64, due time.Time, source *entity.Source) *entity.Bill {
	t.Helper()
	var srcID *uuid.UUID
	if source != nil {
		srcID = &source.ID
	}
	b, err := f.bills.Create(f.ctx, &entity.Bill{
		ID: uuid.New(), Name: name,
		AmountMinMinor: min, AmountMaxMinor: max,
		Cadence: "monthly", NextDueDate: due,
		LinkedSourceID: srcID, Active: true,
	})
	require.NoError(t, err)
	return b
}

func TestLunasPaysAFixedBillInOneMessage(t *testing.T) {
	f := newBotFixture(t, nil)
	today := service.JakartaToday()
	bill := f.bill(t, "Spotify", 54_000, 54_000, today, f.bca)

	f.say(t, "m-lunas-1", "lunas")

	txs := f.live(t)
	require.Len(t, txs, 1, "a band with one value needs no question")
	assert.Equal(t, "expense", txs[0].Type)
	assert.EqualValues(t, 54_000, txs[0].AmountMinor)
	require.NotNil(t, txs[0].Merchant)
	assert.Equal(t, "Spotify", *txs[0].Merchant)

	after, err := f.bills.Get(f.ctx, bill.ID)
	require.NoError(t, err)
	wantNext, ok := service.NextOccurrence("monthly", today, today.Day())
	require.True(t, ok)
	assert.Equal(t, wantNext.Format("2006-01-02"), after.NextDueDate.Format("2006-01-02"))

	p, err := f.billPays.ByTransaction(f.ctx, txs[0].ID)
	require.NoError(t, err)
	require.NotNil(t, p, "the payment is linked so batal can rewind the bill")

	assert.Contains(t, f.sender.last(), "Lunas: Spotify")
	assert.Contains(t, f.sender.reactionsOn("m-lunas-1"), "✅")
}

func TestLunasAsksForTheAmountWhenTheBandIsARange(t *testing.T) {
	f := newBotFixture(t, nil)
	today := service.JakartaToday()
	f.bill(t, "Listrik", 200_000, 300_000, today, f.bca)

	f.say(t, "m-lunas-2", "lunas")
	assert.Contains(t, f.sender.last(), "Berapa nominal untuk Listrik?")
	assert.Empty(t, f.live(t), "a range is not an amount: nothing is written yet")

	f.say(t, "m-lunas-3", "235rb")
	txs := f.live(t)
	require.Len(t, txs, 1)
	assert.EqualValues(t, 235_000, txs[0].AmountMinor)
	assert.Contains(t, f.sender.last(), "Lunas: Listrik")
}

func TestLunasChoosesBetweenDueBills(t *testing.T) {
	f := newBotFixture(t, nil)
	today := service.JakartaToday()
	f.bill(t, "Listrik", 250_000, 250_000, today, f.bca)
	f.bill(t, "Indihome", 350_000, 350_000, today, f.bca)

	f.say(t, "m-lunas-4", "lunas 250rb")
	reply := f.sender.last()
	assert.Contains(t, reply, "1. Listrik")
	assert.Contains(t, reply, "2. Indihome")
	assert.Empty(t, f.live(t), "the bot never picks a bill for the user")

	f.say(t, "m-lunas-5", "2")
	txs := f.live(t)
	require.Len(t, txs, 1)
	require.NotNil(t, txs[0].Merchant)
	assert.Equal(t, "Indihome", *txs[0].Merchant, "the number the user typed picks the bill")
	assert.EqualValues(t, 250_000, txs[0].AmountMinor, "the amount from the first message survives the choice")
}

func TestLunasByName(t *testing.T) {
	f := newBotFixture(t, nil)
	today := service.JakartaToday()
	// Not due for a month: naming a bill is how you pay early.
	f.bill(t, "Kos", 1_500_000, 1_500_000, today.AddDate(0, 0, 25), f.bca)

	f.say(t, "m-lunas-6", "lunas kos")
	txs := f.live(t)
	require.Len(t, txs, 1)
	assert.EqualValues(t, 1_500_000, txs[0].AmountMinor)
}

func TestLunasWithNothingDue(t *testing.T) {
	f := newBotFixture(t, nil)
	f.bill(t, "Kos", 1_500_000, 1_500_000, service.JakartaToday().AddDate(0, 0, 20), f.bca)

	f.say(t, "m-lunas-7", "lunas")
	assert.Contains(t, f.sender.last(), "Tidak ada tagihan yang cocok.")
	assert.Empty(t, f.live(t))
}

func TestLunasAsksWhichSourceWhenTheBillHasNone(t *testing.T) {
	f := newBotFixture(t, nil)
	f.bill(t, "Kos", 1_500_000, 1_500_000, service.JakartaToday(), nil)

	f.say(t, "m-lunas-8", "lunas")
	assert.Contains(t, f.sender.last(), "belum punya sumber dana")
	assert.Empty(t, f.live(t))

	f.say(t, "m-lunas-9", "cash")
	txs := f.live(t)
	require.Len(t, txs, 1)
	require.Len(t, txs[0].Legs, 2)
	var paidFrom *uuid.UUID
	for _, leg := range txs[0].Legs {
		if leg.SourceID != nil {
			paidFrom = leg.SourceID
		}
	}
	require.NotNil(t, paidFrom)
	assert.Equal(t, f.cash.ID, *paidFrom)
}

func TestLunasThenBatalRewindsTheBill(t *testing.T) {
	f := newBotFixture(t, nil)
	today := service.JakartaToday()
	bill := f.bill(t, "Spotify", 54_000, 54_000, today, f.bca)

	f.say(t, "m-lunas-10", "lunas")
	require.Len(t, f.live(t), 1)

	f.say(t, "m-lunas-11", "batal")

	// batal reverses rather than deletes (Principle 5): both rows stay
	// visible and net to zero. What rewinds is the schedule.
	txs := f.live(t)
	require.Len(t, txs, 2)
	require.NotNil(t, txs[1].ReversesTransactionID)
	assert.Equal(t, txs[0].ID, *txs[1].ReversesTransactionID)

	after, err := f.bills.Get(f.ctx, bill.ID)
	require.NoError(t, err)
	assert.Equal(t, today.Format("2006-01-02"), after.NextDueDate.Format("2006-01-02"),
		"undoing the payment puts the occurrence back")

	p, err := f.billPays.ByTransaction(f.ctx, txs[0].ID)
	require.NoError(t, err)
	assert.Nil(t, p, "the link is spent; a second batal cannot rewind twice")
}
