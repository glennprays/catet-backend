package repository

import (
	"context"
	"errors"
	"time"

	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/scope"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// BillRepository persists recurring obligations.
type BillRepository struct {
	db *pgxpool.Pool
}

func NewBillRepository(db *pgxpool.Pool) *BillRepository {
	return &BillRepository{db: db}
}

const billColumns = `
	b.id, b.name, b.amount_min_minor, b.amount_max_minor, b.cadence,
	b.next_due_date, b.linked_source_id, s.name, b.category_id, c.name,
	b.active, b.remaining_count, b.completed_at, b.reminder_offsets,
	b.anchor_day, b.created_at`

// billFrom carries the tenant predicate, not just the joins: every
// caller is scoped, so $1 is always the user and any further condition
// is appended with AND — never a second WHERE.
const billFrom = `
	FROM bills b
	LEFT JOIN sources s ON s.id = b.linked_source_id
	LEFT JOIN categories c ON c.id = b.category_id
	WHERE b.user_id = $1`

// billDest is the scan target shared by every reader: reminder_offsets
// comes back as smallint[], and the entity carries []int so the API
// contract is plain integers.
func billDest(b *entity.Bill, offsets *[]int16) []any {
	return []any{
		&b.ID, &b.Name, &b.AmountMinMinor, &b.AmountMaxMinor, &b.Cadence,
		&b.NextDueDate, &b.LinkedSourceID, &b.LinkedSourceName, &b.CategoryID, &b.CategoryName,
		&b.Active, &b.RemainingCount, &b.CompletedAt, offsets,
		&b.AnchorDay, &b.CreatedAt,
	}
}

// hydrateBill finishes a scanned row: reminder_offsets becomes a
// non-nil []int (the dashboard indexes it, so `null` on the wire would
// be a client crash, not an empty state) and the due date is re-pinned
// to Jakarta midnight so it compares equal to the one the service
// wrote.
func hydrateBill(b *entity.Bill, offsets []int16) {
	b.ReminderOffsets = make([]int, 0, len(offsets))
	for _, o := range offsets {
		b.ReminderOffsets = append(b.ReminderOffsets, int(o))
	}
	b.NextDueDate = jakartaDay(b.NextDueDate)
}

func scanBill(row pgx.Row) (*entity.Bill, error) {
	var b entity.Bill
	var offsets []int16
	err := row.Scan(billDest(&b, &offsets)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	hydrateBill(&b, offsets)
	return &b, nil
}

func (r *BillRepository) Create(ctx context.Context, b *entity.Bill) (*entity.Bill, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, err
	}
	// INSERT ... RETURNING cannot join, so insert then re-read via Get
	// to pick up the joined source/category names.
	var id uuid.UUID
	err = r.db.QueryRow(ctx,
		`INSERT INTO bills (id, user_id, name, amount_min_minor, amount_max_minor, cadence, next_due_date,
		   linked_source_id, category_id, anchor_day, remaining_count, reminder_offsets)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		 RETURNING id`,
		b.ID, uid, b.Name, b.AmountMinMinor, b.AmountMaxMinor, b.Cadence, b.NextDueDate,
		b.LinkedSourceID, b.CategoryID, b.NextDueDate.Day(), b.RemainingCount, toInt16(b.ReminderOffsets),
	).Scan(&id)
	if err != nil {
		return nil, err
	}
	return r.Get(ctx, id)
}

func (r *BillRepository) Get(ctx context.Context, id uuid.UUID) (*entity.Bill, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, err
	}
	return scanBill(r.db.QueryRow(ctx,
		`SELECT `+billColumns+billFrom+` AND b.id = $2`, uid, id,
	))
}

func (r *BillRepository) List(ctx context.Context) ([]entity.Bill, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Query(ctx,
		`SELECT `+billColumns+billFrom+` ORDER BY b.active DESC, b.next_due_date ASC`,
		uid,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return collectBills(rows)
}

// ListActive is the reader every projection uses — reservations, the
// reminder digest, `lunas` candidates: a bill that is off or completed
// is not an obligation.
func (r *BillRepository) ListActive(ctx context.Context) ([]entity.Bill, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Query(ctx,
		`SELECT `+billColumns+billFrom+` AND b.active ORDER BY b.next_due_date ASC`,
		uid,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectBills(rows)
}

func collectBills(rows pgx.Rows) ([]entity.Bill, error) {
	out := []entity.Bill{}
	for rows.Next() {
		var b entity.Bill
		var offsets []int16
		if err := rows.Scan(billDest(&b, &offsets)...); err != nil {
			return nil, err
		}
		hydrateBill(&b, offsets)
		out = append(out, b)
	}
	return out, rows.Err()
}

func toInt16(in []int) []int16 {
	out := make([]int16, 0, len(in))
	for _, v := range in {
		out = append(out, int16(v))
	}
	return out
}

// Update applies any subset; nil = unchanged. Scanning goes through
// Get so the joined names stay fresh.
func (r *BillRepository) Update(ctx context.Context, id uuid.UUID, fields map[string]any) (*entity.Bill, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, err
	}
	// The anchor follows the due date: rescheduling a monthly bill to
	// the 5th must re-anchor it, or the next advance would jump back to
	// the old day-of-month.
	if due, ok := fields["next_due_date"].(time.Time); ok {
		fields["anchor_day"] = due.Day()
	}
	sets := make([]string, 0, len(fields))
	args := []any{id, uid}
	i := 2
	for col, val := range fields {
		i++
		if offsets, ok := val.([]int); ok {
			val = toInt16(offsets) // smallint[] column
		}
		args = append(args, val)
		sets = append(sets, col+" = $"+itoa(i))
	}
	if len(sets) == 0 {
		return r.Get(ctx, id)
	}
	q := `UPDATE bills SET ` + joinComma(sets) + ` WHERE id = $1 AND user_id = $2`
	tag, err := r.db.Exec(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, nil
	}
	return r.Get(ctx, id)
}

// SetActive deactivates a bill. Returns false when missing — another
// tenant's bill is missing.
func (r *BillRepository) SetActive(ctx context.Context, id uuid.UUID, active bool) (bool, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return false, err
	}
	tag, err := r.db.Exec(ctx, `UPDATE bills SET active = $3 WHERE id = $1 AND user_id = $2`, id, uid, active)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// Complete retires a bill: the last occurrence of a `once` bill or the
// last unit of a tenor was paid. Separate from SetActive because a
// completed bill must not be reactivatable.
func (r *BillRepository) Complete(ctx context.Context, id uuid.UUID, at time.Time, decrement bool) (bool, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return false, err
	}
	tag, err := r.db.Exec(ctx,
		`UPDATE bills
		 SET active = false,
		     completed_at = $3,
		     remaining_count = CASE WHEN $4 AND remaining_count IS NOT NULL THEN remaining_count - 1 ELSE remaining_count END
		 WHERE id = $1 AND user_id = $2`,
		id, uid, at, decrement,
	)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// Advance moves a bill to its next occurrence, optimistically guarded
// on the due date the caller paid: if the user edited the schedule
// between read and write, nothing moves and the caller unwinds.
func (r *BillRepository) Advance(ctx context.Context, id uuid.UUID, from, to time.Time, decrement bool) (bool, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return false, err
	}
	tag, err := r.db.Exec(ctx,
		`UPDATE bills
		 SET next_due_date = $4,
		     remaining_count = CASE WHEN $5 AND remaining_count IS NOT NULL THEN remaining_count - 1 ELSE remaining_count END
		 WHERE id = $1 AND user_id = $2 AND next_due_date = $3`,
		id, uid, from, to, decrement,
	)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// Revert undoes an Advance/Complete when the settling transaction is
// voided. The guard is the same optimistic one: only revert a bill that
// still sits where the payment left it, so a manual reschedule after
// the payment wins over the undo.
func (r *BillRepository) Revert(ctx context.Context, id uuid.UUID, advancedTo *time.Time, dueDate time.Time, decremented bool) (bool, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return false, err
	}
	tag, err := r.db.Exec(ctx,
		`UPDATE bills
		 SET next_due_date = $3,
		     active = true,
		     completed_at = NULL,
		     remaining_count = CASE WHEN $5 AND remaining_count IS NOT NULL THEN remaining_count + 1 ELSE remaining_count END
		 WHERE id = $1 AND user_id = $2
		   AND CASE WHEN $4::date IS NULL THEN completed_at IS NOT NULL ELSE next_due_date = $4::date END`,
		id, uid, dueDate, advancedTo, decremented,
	)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// CountOverdue counts active bills whose due date has already passed —
// the badge behind the red "terlambat" attention item.
func (r *BillRepository) CountOverdue(ctx context.Context, today time.Time) (int64, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return 0, err
	}
	var n int64
	err = r.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM bills WHERE user_id = $1 AND active AND next_due_date < $2`,
		uid, today,
	).Scan(&n)
	return n, err
}
