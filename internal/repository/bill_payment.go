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

// BillPaymentRepository records which transaction settled which bill
// occurrence — the link that lets a void rewind the schedule.
type BillPaymentRepository struct {
	db *pgxpool.Pool
}

func NewBillPaymentRepository(db *pgxpool.Pool) *BillPaymentRepository {
	return &BillPaymentRepository{db: db}
}

const billPaymentColumns = `
	p.id, p.bill_id, b.name, p.transaction_id, p.due_date, p.advanced_to,
	p.amount_minor, p.decremented, p.paid_at`

const billPaymentFrom = `
	FROM bill_payments p
	JOIN bills b ON b.id = p.bill_id
	WHERE p.user_id = $1`

func scanBillPayment(row pgx.Row) (*entity.BillPayment, error) {
	var p entity.BillPayment
	err := row.Scan(
		&p.ID, &p.BillID, &p.BillName, &p.TransactionID, &p.DueDate, &p.AdvancedTo,
		&p.AmountMinor, &p.Decremented, &p.PaidAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.DueDate = jakartaDay(p.DueDate)
	p.AdvancedTo = jakartaDayPtr(p.AdvancedTo)
	return &p, nil
}

func (r *BillPaymentRepository) Create(ctx context.Context, p *entity.BillPayment) error {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return err
	}
	_, err = r.db.Exec(ctx,
		`INSERT INTO bill_payments (id, user_id, bill_id, transaction_id, due_date, advanced_to, amount_minor, decremented)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		p.ID, uid, p.BillID, p.TransactionID, p.DueDate, p.AdvancedTo, p.AmountMinor, p.Decremented,
	)
	return err
}

// ByTransaction answers "did this transaction settle a bill?" — nil
// when it did not, which is the common case.
func (r *BillPaymentRepository) ByTransaction(ctx context.Context, txID uuid.UUID) (*entity.BillPayment, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, err
	}
	return scanBillPayment(r.db.QueryRow(ctx,
		`SELECT `+billPaymentColumns+billPaymentFrom+` AND p.transaction_id = $2`, uid, txID,
	))
}

func (r *BillPaymentRepository) Delete(ctx context.Context, id uuid.UUID) error {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return err
	}
	_, err = r.db.Exec(ctx, `DELETE FROM bill_payments WHERE id = $1 AND user_id = $2`, id, uid)
	return err
}

// ListBetween feeds the calendar's paid markers: payments settled in
// [from, to). Never nil.
func (r *BillPaymentRepository) ListBetween(ctx context.Context, from, to time.Time) ([]entity.BillPayment, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Query(ctx,
		`SELECT `+billPaymentColumns+billPaymentFrom+` AND p.paid_at >= $2 AND p.paid_at < $3
		 ORDER BY p.paid_at ASC`,
		uid, from, to,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []entity.BillPayment{}
	for rows.Next() {
		var p entity.BillPayment
		if err := rows.Scan(
			&p.ID, &p.BillID, &p.BillName, &p.TransactionID, &p.DueDate, &p.AdvancedTo,
			&p.AmountMinor, &p.Decremented, &p.PaidAt,
		); err != nil {
			return nil, err
		}
		p.DueDate = jakartaDay(p.DueDate)
		p.AdvancedTo = jakartaDayPtr(p.AdvancedTo)
		out = append(out, p)
	}
	return out, rows.Err()
}
