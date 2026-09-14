package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Cursor is the decoded keyset position: strictly rows after
// (occurred_at, id) in DESC order.
type Cursor struct {
	At time.Time
	ID uuid.UUID
}

// ListFilter mirrors the GET /transactions query parameters.
type ListFilter struct {
	SourceID     *uuid.UUID
	CategoryID   *uuid.UUID
	ReviewStatus *string
	Query        *string
	From         *time.Time
	To           *time.Time
	Direction    *string
	Limit        int
	Cursor       *Cursor
}

// LedgerRow is the joined list row ready for response.LedgerItem.
type LedgerRow struct {
	TransactionID         uuid.UUID
	OccurredAt            time.Time
	Type                  string
	Merchant              *string
	Note                  *string
	AmountMinor           int64
	CategoryID            *uuid.UUID
	CategoryName          *string
	SourceID              uuid.UUID
	SourceName            string
	ReviewStatus          string
	CategorySource        *string
	CategoryConfidence    *float64
	Voided                bool
	ReversesTransactionID *uuid.UUID
}

// TransactionRepository persists two-leg transactions atomically.
type TransactionRepository struct {
	db *pgxpool.Pool
}

func NewTransactionRepository(db *pgxpool.Pool) *TransactionRepository {
	return &TransactionRepository{db: db}
}

// Insert writes the transaction header and its legs in one database
// transaction. The caller assigns IDs.
func (r *TransactionRepository) Insert(ctx context.Context, tx *entity.Transaction) (*entity.Transaction, error) {
	pgTx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = pgTx.Rollback(ctx) }()

	editHistory := tx.EditHistory
	if len(editHistory) == 0 {
		editHistory = json.RawMessage("[]")
	}

	created, err := pgTx.Exec(ctx,
		`INSERT INTO transactions (
			id, occurred_at, origin, type, merchant, note, category_id,
			category_source, category_confidence, review_status, external_id,
			content_hash, reverses_transaction_id, edit_history, voided_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		tx.ID, tx.OccurredAt, tx.Origin, tx.Type, tx.Merchant, tx.Note, tx.CategoryID,
		tx.CategorySource, tx.CategoryConfidence, tx.ReviewStatus, tx.ExternalID,
		tx.ContentHash, tx.ReversesTransactionID, editHistory, tx.VoidedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("insert transaction: %w", err)
	}
	if created.RowsAffected() != 1 {
		return nil, fmt.Errorf("insert transaction: %d rows affected", created.RowsAffected())
	}
	if err := insertLegs(ctx, pgTx, tx.ID, tx.Legs); err != nil {
		return nil, err
	}
	if err := pgTx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return r.Get(ctx, tx.ID)
}

// Save replaces the mutable columns and the leg set atomically (PATCH).
func (r *TransactionRepository) Save(ctx context.Context, tx *entity.Transaction) (*entity.Transaction, error) {
	pgTx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = pgTx.Rollback(ctx) }()

	tag, err := pgTx.Exec(ctx,
		`UPDATE transactions SET
			occurred_at = $2, merchant = $3, note = $4, category_id = $5,
			category_source = $6, review_status = $7, edit_history = $8
		 WHERE id = $1 AND voided_at IS NULL`,
		tx.ID, tx.OccurredAt, tx.Merchant, tx.Note, tx.CategoryID,
		tx.CategorySource, tx.ReviewStatus, tx.EditHistory,
	)
	if err != nil {
		return nil, fmt.Errorf("update transaction: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return nil, pgx.ErrNoRows
	}
	if _, err := pgTx.Exec(ctx, `DELETE FROM legs WHERE transaction_id = $1`, tx.ID); err != nil {
		return nil, fmt.Errorf("delete legs: %w", err)
	}
	if err := insertLegs(ctx, pgTx, tx.ID, tx.Legs); err != nil {
		return nil, err
	}
	if err := pgTx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return r.Get(ctx, tx.ID)
}

// SetVoided soft-voids a transaction. Returns false when the row does
// not exist, true when it was voided (idempotent on already-voided).
func (r *TransactionRepository) SetVoided(ctx context.Context, id uuid.UUID, at time.Time) (bool, error) {
	tag, err := r.db.Exec(ctx,
		`UPDATE transactions SET voided_at = $2 WHERE id = $1 AND voided_at IS NULL`,
		id, at,
	)
	if err != nil {
		return false, fmt.Errorf("void transaction: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

const txDetailSelect = `
	SELECT t.id, t.occurred_at, t.origin, t.type, t.merchant, t.note,
	       t.category_id, c.name, t.category_source, t.category_confidence,
	       t.review_status, t.external_id, t.reverses_transaction_id,
	       COALESCE(t.edit_history, '[]'::jsonb), t.voided_at, t.created_at
	FROM transactions t
	LEFT JOIN categories c ON c.id = t.category_id`

// Get loads one transaction with its legs (and display names).
func (r *TransactionRepository) Get(ctx context.Context, id uuid.UUID) (*entity.Transaction, error) {
	row := r.db.QueryRow(ctx, txDetailSelect+` WHERE t.id = $1`, id)
	var (
		t           entity.Transaction
		editHistory []byte
	)
	err := row.Scan(
		&t.ID, &t.OccurredAt, &t.Origin, &t.Type, &t.Merchant, &t.Note,
		&t.CategoryID, &t.CategoryName, &t.CategorySource, &t.CategoryConfidence,
		&t.ReviewStatus, &t.ExternalID, &t.ReversesTransactionID,
		&editHistory, &t.VoidedAt, &t.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t.EditHistory = json.RawMessage(editHistory)

	legs, err := r.legs(ctx, id)
	if err != nil {
		return nil, err
	}
	t.Legs = legs

	// Unsigned base amount: the largest absolute leg (both legs share
	// the same magnitude).
	for _, l := range legs {
		abs := l.SignedAmount
		if abs < 0 {
			abs = -abs
		}
		if abs > t.AmountMinor {
			t.AmountMinor = abs
		}
	}
	return &t, nil
}

func (r *TransactionRepository) legs(ctx context.Context, txID uuid.UUID) ([]entity.Leg, error) {
	rows, err := r.db.Query(ctx,
		`SELECT l.id, l.transaction_id, l.source_id, s.name, l.category_id, c.name, l.signed_amount
		 FROM legs l
		 LEFT JOIN sources s ON s.id = l.source_id
		 LEFT JOIN categories c ON c.id = l.category_id
		 WHERE l.transaction_id = $1
		 ORDER BY l.signed_amount ASC, l.id ASC`,
		txID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []entity.Leg{}
	for rows.Next() {
		var l entity.Leg
		if err := rows.Scan(&l.ID, &l.TransactionID, &l.SourceID, &l.SourceName, &l.CategoryID, &l.CategoryName, &l.SignedAmount); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func insertLegs(ctx context.Context, tx pgx.Tx, txID uuid.UUID, legs []entity.Leg) error {
	if len(legs) != 2 {
		return fmt.Errorf("transaction %s must have exactly two legs, got %d", txID, len(legs))
	}
	for _, leg := range legs {
		if _, err := tx.Exec(ctx,
			`INSERT INTO legs (id, transaction_id, source_id, category_id, signed_amount)
			 VALUES ($1, $2, $3, $4, $5)`,
			leg.ID, txID, leg.SourceID, leg.CategoryID, leg.SignedAmount,
		); err != nil {
			return fmt.Errorf("insert leg: %w", err)
		}
	}
	return nil
}

// ledgerJoin is the shared FROM/WHERE builder for List and Count. The
// LATERAL join picks the primary source leg: the only source leg for
// expense/income/adjustment, the outbound (negative) leg for transfers
// — giving user-perspective signed amounts.
const ledgerSelect = `
	SELECT t.id, t.occurred_at, t.type, t.merchant, t.note,
	       p.signed_amount, t.category_id, c.name, p.source_id, ps.name,
	       t.review_status, t.category_source, t.category_confidence,
	       t.voided_at IS NOT NULL, t.reverses_transaction_id
	FROM transactions t
	JOIN LATERAL (
		SELECT l.source_id, l.signed_amount FROM legs l
		WHERE l.transaction_id = t.id AND l.source_id IS NOT NULL
		ORDER BY l.signed_amount ASC, l.id ASC LIMIT 1
	) p ON true
	LEFT JOIN sources ps ON ps.id = p.source_id
	LEFT JOIN categories c ON c.id = t.category_id`

func (r *TransactionRepository) buildWhere(f ListFilter, args []any) (string, []any) {
	conds := []string{"1 = 1"}
	add := func(cond string, val any) {
		args = append(args, val)
		conds = append(conds, fmt.Sprintf("%s $%d", cond, len(args)))
	}

	if f.SourceID != nil {
		conds = append(conds, `EXISTS (SELECT 1 FROM legs ls WHERE ls.transaction_id = t.id AND ls.source_id = $`+fmt.Sprint(len(args)+1)+`)`)
		args = append(args, *f.SourceID)
	}
	if f.CategoryID != nil {
		add("t.category_id =", *f.CategoryID)
	}
	if f.ReviewStatus != nil {
		add("t.review_status =", *f.ReviewStatus)
	}
	if f.Query != nil {
		args = append(args, "%"+*f.Query+"%")
		conds = append(conds, fmt.Sprintf("(t.merchant ILIKE $%d OR t.note ILIKE $%d)", len(args), len(args)))
	}
	if f.From != nil {
		add("t.occurred_at >=", *f.From)
	}
	if f.To != nil {
		add("t.occurred_at <", *f.To)
	}
	if f.Direction != nil {
		add("t.type =", *f.Direction)
	}
	if f.Cursor != nil {
		args = append(args, f.Cursor.At, f.Cursor.ID)
		n := len(args)
		conds = append(conds, fmt.Sprintf("(t.occurred_at, t.id) < ($%d, $%d)", n-1, n))
	}

	return " WHERE " + strings.Join(conds, " AND "), args
}

// List returns one keyset page ordered (occurred_at, id) DESC.
func (r *TransactionRepository) List(ctx context.Context, f ListFilter) ([]LedgerRow, error) {
	args := []any{f.Limit}
	where, args := r.buildWhere(f, args)
	sql := ledgerSelect + where + ` ORDER BY t.occurred_at DESC, t.id DESC LIMIT $1`

	rows, err := r.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("list transactions: %w", err)
	}
	defer rows.Close()

	out := []LedgerRow{}
	for rows.Next() {
		var row LedgerRow
		if err := rows.Scan(
			&row.TransactionID, &row.OccurredAt, &row.Type, &row.Merchant, &row.Note,
			&row.AmountMinor, &row.CategoryID, &row.CategoryName, &row.SourceID, &row.SourceName,
			&row.ReviewStatus, &row.CategorySource, &row.CategoryConfidence,
			&row.Voided, &row.ReversesTransactionID,
		); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// Count reports the total rows matching the filter (ignores cursor/limit).
func (r *TransactionRepository) Count(ctx context.Context, f ListFilter) (int64, error) {
	args := []any{}
	where, args := r.buildWhere(f, args)
	var total int64
	err := r.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM transactions t`+where, args...,
	).Scan(&total)
	return total, err
}
