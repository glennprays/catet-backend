package repository

import (
	"context"
	"time"

	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/scope"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// BillReminderSendRepository is the dedup ledger behind the reminder
// digest: one row per (bill, due date, offset) the user was told about.
// The UNIQUE constraint — not the scheduler's memory — is what makes a
// second tick, a restart, or a second process harmless.
type BillReminderSendRepository struct {
	db *pgxpool.Pool
}

func NewBillReminderSendRepository(db *pgxpool.Pool) *BillReminderSendRepository {
	return &BillReminderSendRepository{db: db}
}

// InsertAll claims the slots for one digest inside the caller's
// transaction: claim first, send, commit. A send that fails rolls the
// claims back so the next tick retries.
func (r *BillReminderSendRepository) InsertAll(ctx context.Context, tx pgx.Tx, rows []entity.BillReminderSend) error {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if _, err := tx.Exec(ctx,
			`INSERT INTO bill_reminder_sends (id, user_id, bill_id, due_date, offset_days, send_date)
			 VALUES ($1,$2,$3,$4,$5,$6)
			 ON CONFLICT (bill_id, due_date, offset_days) DO NOTHING`,
			uuid.New(), uid, row.BillID, row.DueDate, row.OffsetDays, row.SendDate,
		); err != nil {
			return err
		}
	}
	return nil
}

// SentSlots returns the slots already delivered for the given send
// date, keyed "billID|yyyy-mm-dd|offset".
func (r *BillReminderSendRepository) SentSlots(ctx context.Context, sendDate time.Time) (map[string]bool, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Query(ctx,
		`SELECT bill_id, due_date, offset_days FROM bill_reminder_sends
		 WHERE user_id = $1 AND send_date = $2`,
		uid, sendDate,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var billID uuid.UUID
		var due time.Time
		var offset int16
		if err := rows.Scan(&billID, &due, &offset); err != nil {
			return nil, err
		}
		out[SlotKey(billID, due, int(offset))] = true
	}
	return out, rows.Err()
}

// SlotKey is the shared spelling of a reminder slot; the digest builder
// and the dedup reader must agree on it.
func SlotKey(billID uuid.UUID, due time.Time, offset int) string {
	return billID.String() + "|" + due.Format("2006-01-02") + "|" + itoa(offset)
}

// RemindedBillIDs lists the bills named in today's digest — the extra
// `lunas` candidates, so "lunas" right after a reminder resolves to the
// bill the user just read about even when it is not due yet.
func (r *BillReminderSendRepository) RemindedBillIDs(ctx context.Context, sendDate time.Time) ([]uuid.UUID, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Query(ctx,
		`SELECT DISTINCT bill_id FROM bill_reminder_sends WHERE user_id = $1 AND send_date = $2`,
		uid, sendDate,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
