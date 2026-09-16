package service

import (
	"context"
	"time"

	"github.com/glennprays/catet-backend/internal/bot/tmpl"
	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/gateway"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/glennprays/catet-backend/internal/scope"
	"github.com/glennprays/log"
	waga "github.com/glennprays/whatsapp-gateway-sdk-go"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ReminderService delivers the one-a-day bill digest (ADR-0006): the
// single place the bot is allowed to speak first, and only about bills
// the user opted in per bill. It never states an amount.
type ReminderService struct {
	users    *repository.UserRepository
	bills    *repository.BillRepository
	sends    *repository.BillReminderSendRepository
	settings *repository.SettingsRepository
	gw       gateway.Sender
	pool     *pgxpool.Pool
	log      *log.Logger
	// now is injectable so the digest can be tested at a fixed hour
	// without sleeping through a day.
	now func() time.Time
}

func NewReminderService(
	users *repository.UserRepository,
	bills *repository.BillRepository,
	sends *repository.BillReminderSendRepository,
	settings *repository.SettingsRepository,
	gw gateway.Sender,
	pool *pgxpool.Pool,
	logger *log.Logger,
) *ReminderService {
	return &ReminderService{
		users: users, bills: bills, sends: sends, settings: settings,
		gw: gw, pool: pool, log: logger, now: time.Now,
	}
}

// SetNow overrides the clock. Tests only.
func (s *ReminderService) SetNow(now func() time.Time) { s.now = now }

// Run ticks until the context is cancelled. A tick that fails is logged
// and forgotten: a reminder is a courtesy, never a reason to take the
// API down.
func (s *ReminderService) Run(ctx context.Context, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.RunOnce(ctx); err != nil {
				s.log.Warn("reminders", "bill reminder tick failed", map[string]any{"error": err.Error()})
			}
		}
	}
}

// RunOnce sends every digest that is due right now. Only today's slots
// are ever considered: after an outage the user gets today's bills, not
// a back-filled pile of yesterday's.
func (s *ReminderService) RunOnce(ctx context.Context) error {
	nowJ := s.now().In(jakartaLoc)
	today := time.Date(nowJ.Year(), nowJ.Month(), nowJ.Day(), 0, 0, 0, 0, jakartaLoc)

	users, err := s.users.List(ctx)
	if err != nil {
		return err
	}
	for i := range users {
		u := users[i]
		if u.DeactivatedAt != nil {
			continue
		}
		if err := s.runUser(scope.WithUserID(ctx, u.ID), u, nowJ, today); err != nil {
			s.log.Warn("reminders", "bill digest failed", map[string]any{"user_id": u.ID.String(), "error": err.Error()})
		}
	}
	return nil
}

func (s *ReminderService) runUser(ctx context.Context, u entity.User, nowJ, today time.Time) error {
	hour, err := s.settings.GetReminderHour(ctx)
	if err != nil {
		return err
	}
	if nowJ.Hour() < hour {
		return nil
	}

	bills, err := s.bills.ListActive(ctx)
	if err != nil {
		return err
	}
	sent, err := s.sends.SentSlots(ctx, today)
	if err != nil {
		return err
	}
	digest, slots := BuildDigest(bills, today, sent)
	if len(slots) == 0 {
		return nil
	}

	// Indonesian for everyone: there is no per-user locale, and a
	// reminder is not the place to guess one.
	text := tmpl.Render(tmpl.LangID, "bill_reminder", digest)
	if text == "" {
		return nil
	}

	// Claim the slots, send, then commit. A failed send rolls the claims
	// back so the next tick retries; a committed-but-lost send is caught
	// by the gateway idempotency key.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := s.sends.InsertAll(ctx, tx, slots); err != nil {
		return err
	}
	chat := waga.FormatMSISDN(gateway.DigitsOnly(u.Phone))
	key := "bill-reminder:" + u.ID.String() + ":" + today.Format("2006-01-02")
	if _, err := s.gw.SendText(ctx, chat, text, key); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Digest is the rendered content of one daily reminder: names only,
// grouped by how close the due date is.
type Digest struct {
	Overdue  []string
	Today    []string
	Tomorrow []string
	InThree  []string
}

// reminderWindow is how far ahead the digest looks: yesterday (the
// single overdue notice) through three days out (the furthest offset).
const (
	reminderLookBehind = -1
	reminderLookAhead  = 4
)

// BuildDigest is the pure half: which bills go in today's message, and
// which dedup slots that claims. A bill appears only for an offset its
// owner ticked, plus exactly one overdue notice the day after its own
// due date — so a forgotten bill nags once, not daily.
func BuildDigest(bills []entity.Bill, today time.Time, sent map[string]bool) (Digest, []entity.BillReminderSend) {
	var d Digest
	slots := make([]entity.BillReminderSend, 0, 4)

	from := today.AddDate(0, 0, reminderLookBehind)
	to := today.AddDate(0, 0, reminderLookAhead)

	for i := range bills {
		b := bills[i]
		if !b.Active || len(b.ReminderOffsets) == 0 {
			continue
		}
		for _, due := range Occurrences(b, from, to) {
			offset := daysBetween(today, due)
			overdue := offset == reminderLookBehind && due.Equal(b.NextDueDate)
			if !overdue && !containsInt(b.ReminderOffsets, offset) {
				continue
			}
			if sent[repository.SlotKey(b.ID, due, offset)] {
				continue
			}
			switch offset {
			case -1:
				d.Overdue = append(d.Overdue, b.Name)
			case 0:
				d.Today = append(d.Today, b.Name)
			case 1:
				d.Tomorrow = append(d.Tomorrow, b.Name)
			case 3:
				d.InThree = append(d.InThree, b.Name)
			default:
				continue
			}
			slots = append(slots, entity.BillReminderSend{
				BillID:     b.ID,
				DueDate:    due,
				OffsetDays: offset,
				SendDate:   today,
			})
		}
	}
	return d, slots
}

// daysBetween counts whole calendar days from a to b; both are
// Jakarta midnights, so the subtraction cannot drift.
func daysBetween(a, b time.Time) int {
	return int(b.Sub(a).Hours() / 24)
}

func containsInt(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
