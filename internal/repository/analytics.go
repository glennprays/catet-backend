package repository

import (
	"context"
	"time"

	"github.com/glennprays/catet-backend/internal/scope"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AnalyticsRepository runs the deterministic aggregation queries. All
// day bucketing happens in Asia/Jakarta; every range is [from, to).
// Legs carry no tenant of their own, so each aggregate is filtered on
// the transaction it hangs off.
type AnalyticsRepository struct {
	db *pgxpool.Pool
}

func NewAnalyticsRepository(db *pgxpool.Pool) *AnalyticsRepository {
	return &AnalyticsRepository{db: db}
}

// CategorySpendRow is one category's direct expense in a range.
type CategorySpendRow struct {
	CategoryID uuid.UUID
	Name       string
	Essential  bool
	ParentID   *uuid.UUID
	Amount     int64
}

// CategorySpend returns direct expense per category (the positive
// category legs of non-voided expense transactions). Reversals carry
// mirrored negative legs and cancel out naturally.
func (r *AnalyticsRepository) CategorySpend(ctx context.Context, from, to time.Time) ([]CategorySpendRow, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Query(ctx,
		`SELECT c.id, c.name, c.essential, c.parent_id, COALESCE(SUM(l.signed_amount), 0)
		 FROM legs l
		 JOIN transactions t ON t.id = l.transaction_id
		 JOIN categories c ON c.id = l.category_id
		 WHERE t.user_id = $1
		   AND t.type = 'expense' AND t.voided_at IS NULL AND l.signed_amount > 0
		   AND t.occurred_at >= $2 AND t.occurred_at < $3
		 GROUP BY c.id, c.name, c.essential, c.parent_id`,
		uid, from, to,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []CategorySpendRow{}
	for rows.Next() {
		var row CategorySpendRow
		if err := rows.Scan(&row.CategoryID, &row.Name, &row.Essential, &row.ParentID, &row.Amount); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// dailyFlowRow is one Jakarta day's income/expense total (source-side
// legs; transfers excluded from spend math).
type dailyFlowRow struct {
	Date string
	Flow int64
}

// DailyFlows returns per-Jakarta-day sums keyed "YYYY-MM-DD" for the
// requested flow kind (income = positive income source legs, expense =
// negative expense source legs as positive magnitudes).
func (r *AnalyticsRepository) DailyFlows(ctx context.Context, from, to time.Time) (map[string]int64, map[string]int64, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, nil, err
	}
	rows, err := r.db.Query(ctx,
		`SELECT (t.occurred_at AT TIME ZONE 'Asia/Jakarta')::date::text AS d,
		        t.type,
		        COALESCE(SUM(CASE WHEN l.signed_amount < 0 THEN -l.signed_amount ELSE l.signed_amount END), 0)
		 FROM legs l
		 JOIN transactions t ON t.id = l.transaction_id
		 WHERE t.user_id = $1
		   AND t.voided_at IS NULL AND l.source_id IS NOT NULL
		   AND t.type IN ('income', 'expense')
		   AND t.occurred_at >= $2 AND t.occurred_at < $3
		 GROUP BY d, t.type
		 ORDER BY d`,
		uid, from, to,
	)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	income := map[string]int64{}
	expense := map[string]int64{}
	for rows.Next() {
		var row dailyFlowRow
		var typ string
		if err := rows.Scan(&row.Date, &typ, &row.Flow); err != nil {
			return nil, nil, err
		}
		if typ == "income" {
			income[row.Date] = row.Flow
		} else {
			expense[row.Date] = row.Flow
		}
	}
	return income, expense, rows.Err()
}

// TopMerchants ranks merchants by outflow (negative expense source
// legs as positive magnitudes) in range. Merchants without a name are
// skipped — they are not merchants.
func (r *AnalyticsRepository) TopMerchants(ctx context.Context, from, to time.Time, limit int) ([]TopMerchantRow, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Query(ctx,
		`SELECT t.merchant,
		        COALESCE(SUM(-l.signed_amount), 0) AS amount,
		        COUNT(DISTINCT t.id) AS tx_count
		 FROM legs l
		 JOIN transactions t ON t.id = l.transaction_id
		 WHERE t.user_id = $1
		   AND t.type = 'expense' AND t.voided_at IS NULL
		   AND l.source_id IS NOT NULL AND l.signed_amount < 0
		   AND t.merchant IS NOT NULL AND t.merchant <> ''
		   AND t.occurred_at >= $2 AND t.occurred_at < $3
		 GROUP BY t.merchant
		 ORDER BY amount DESC, t.merchant ASC
		 LIMIT $4`,
		uid, from, to, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []TopMerchantRow{}
	for rows.Next() {
		var m TopMerchantRow
		if err := rows.Scan(&m.Name, &m.AmountMinor, &m.TxCount); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

type TopMerchantRow struct {
	Name        string
	AmountMinor int64
	TxCount     int64
}

// NeedsReviewCount counts live transactions in the needs-review queue.
func (r *AnalyticsRepository) NeedsReviewCount(ctx context.Context) (int64, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return 0, err
	}
	var n int64
	err = r.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM transactions
		 WHERE user_id = $1 AND review_status = 'needs_review' AND voided_at IS NULL`,
		uid,
	).Scan(&n)
	return n, err
}

// UncategorizedCount counts live expense/income captures still missing a
// category since `from` — the number the bot's rekap reports.
func (r *AnalyticsRepository) UncategorizedCount(ctx context.Context, from time.Time) (int64, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return 0, err
	}
	var n int64
	err = r.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM transactions
		 WHERE user_id = $1 AND category_id IS NULL AND type IN ('expense','income')
		   AND voided_at IS NULL AND occurred_at >= $2`,
		uid, from,
	).Scan(&n)
	return n, err
}
