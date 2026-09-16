package service_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/glennprays/catet-backend/internal/service"
	"github.com/glennprays/catet-backend/internal/testkit"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type sentReminder struct{ chat, text, key string }

// recordingSender stands in for the WhatsApp gateway: the digest is an
// outbound message, so what it says and how often is the whole contract.
type recordingSender struct {
	mu   sync.Mutex
	sent []sentReminder
}

func (s *recordingSender) SendText(_ context.Context, chat, text, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, sentReminder{chat: chat, text: text, key: key})
	return uuid.NewString(), nil
}

func (s *recordingSender) React(context.Context, string, string, string, string) error { return nil }
func (s *recordingSender) SetTyping(context.Context, string, bool) error               { return nil }
func (s *recordingSender) ContactLID(context.Context, string) (string, error)          { return "", nil }

func (s *recordingSender) all() []sentReminder {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]sentReminder, len(s.sent))
	copy(out, s.sent)
	return out
}

func TestReminderDigest(t *testing.T) {
	pool := testkit.NewDB(t)
	ctx, userID := testkit.Scoped(t, pool, "+628111000031", "Reminders")

	billRepo := repository.NewBillRepository(pool)
	sendRepo := repository.NewBillReminderSendRepository(pool)
	settingsRepo := repository.NewSettingsRepository(pool)
	sender := &recordingSender{}
	svc := service.NewReminderService(
		repository.NewUserRepository(pool), billRepo, sendRepo, settingsRepo,
		sender, pool, testkit.Logger(t),
	)

	today := service.JakartaToday()
	seed := func(name string, due time.Time, offsets []int) *entity.Bill {
		b, err := billRepo.Create(ctx, &entity.Bill{
			ID: uuid.New(), Name: name,
			AmountMinMinor: 100_000, AmountMaxMinor: 100_000,
			Cadence: "monthly", NextDueDate: due, Active: true,
			ReminderOffsets: offsets,
		})
		require.NoError(t, err)
		return b
	}
	seed("Listrik", today, []int{0, 1})
	seed("Spotify", today.AddDate(0, 0, 1), []int{1, 0})
	seed("Kos", today.AddDate(0, 0, 3), []int{3})
	seed("Indihome", today.AddDate(0, 0, -1), []int{0})
	seed("Netflix", today, nil) // opted out: never named

	require.NoError(t, settingsRepo.SetReminderHour(ctx, 8))

	// Before the user's hour, the bot stays quiet.
	svc.SetNow(func() time.Time { return today.Add(7*time.Hour + 59*time.Minute) })
	require.NoError(t, svc.RunOnce(context.Background()))
	assert.Empty(t, sender.all(), "a digest before the send hour is the bot messaging first")

	svc.SetNow(func() time.Time { return today.Add(8 * time.Hour) })
	require.NoError(t, svc.RunOnce(context.Background()))

	sent := sender.all()
	require.Len(t, sent, 1, "every due bill collapses into one message a day")
	text := sent[0].text
	assert.Contains(t, text, "Terlambat: Indihome")
	assert.Contains(t, text, "Hari ini: Listrik")
	assert.Contains(t, text, "Besok: Spotify")
	assert.Contains(t, text, "3 hari lagi: Kos")
	assert.NotContains(t, text, "Netflix", "a bill with no offsets is never named")
	assert.NotContains(t, text, "Rp", "a reminder never states an amount (ADR-0006)")
	assert.Equal(t, "bill-reminder:"+userID.String()+":"+today.Format("2006-01-02"), sent[0].key)

	// A second tick in the same day says nothing new.
	svc.SetNow(func() time.Time { return today.Add(9 * time.Hour) })
	require.NoError(t, svc.RunOnce(context.Background()))
	assert.Len(t, sender.all(), 1, "the slot ledger, not the ticker's memory, is the dedup")

	// Tomorrow: Listrik is now a day overdue and Spotify is due today.
	// Indihome already had its single overdue notice and stays silent.
	tomorrow := today.AddDate(0, 0, 1)
	svc.SetNow(func() time.Time { return tomorrow.Add(8 * time.Hour) })
	require.NoError(t, svc.RunOnce(context.Background()))

	sent = sender.all()
	require.Len(t, sent, 2)
	next := sent[1].text
	assert.Contains(t, next, "Terlambat: Listrik")
	assert.Contains(t, next, "Hari ini: Spotify", "a bill reminded yesterday is reminded again on the day it opted into")
	assert.False(t, strings.Contains(next, "Indihome"),
		"an overdue bill nags once, the day after it was due — not every day")
}

func TestReminderSkipsDeactivatedAndOptedOutAccounts(t *testing.T) {
	pool := testkit.NewDB(t)
	ctx, _ := testkit.Scoped(t, pool, "+628111000032", "Quiet")

	billRepo := repository.NewBillRepository(pool)
	sender := &recordingSender{}
	svc := service.NewReminderService(
		repository.NewUserRepository(pool), billRepo,
		repository.NewBillReminderSendRepository(pool),
		repository.NewSettingsRepository(pool),
		sender, pool, testkit.Logger(t),
	)

	today := service.JakartaToday()
	_, err := billRepo.Create(ctx, &entity.Bill{
		ID: uuid.New(), Name: "Listrik",
		AmountMinMinor: 1, AmountMaxMinor: 1,
		Cadence: "monthly", NextDueDate: today, Active: true,
	})
	require.NoError(t, err)

	svc.SetNow(func() time.Time { return today.Add(12 * time.Hour) })
	require.NoError(t, svc.RunOnce(context.Background()))
	assert.Empty(t, sender.all(), "silence is the default: no offsets, no message")
}
