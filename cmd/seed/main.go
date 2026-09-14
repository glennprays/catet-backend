// Command seed rebuilds a deterministic development dataset: sources
// with opening balances, a category tree, a month of transactions,
// bills, budgets, and one parked bot action.
//
// It is destructive by design (dev data only) and refuses to run
// against a database that is not the configured development one.
// Dates are anchored to today in Asia/Jakarta so bill reminders and
// the Overview attention strip stay meaningful on every run.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var jakarta = time.FixedZone("WIB", 7*60*60)

type sourceSpec struct {
	name    string
	kind    string
	isDebt  bool
	opening int64
}

type txSpec struct {
	kind        string // expense | income | transfer
	amount      int64
	source      string
	destination string
	category    string
	merchant    string
	daysAgo     int
	needsReview bool
	confidence  float64
}

func main() {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		// Local-only dev default (matches .env.example); not a secret.
		url = "postgres://postgres:catet_dev@localhost:5432/catet_dev?sslmode=disable" // #nosec G101
	}
	if strings.Contains(url, "_test") {
		log.Fatal("seed: refusing to write to a test database")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		log.Fatalf("seed: connect: %v (is postgres running? see make run-dev)", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	if err := seed(ctx, conn); err != nil {
		log.Fatalf("seed: %v", err)
	}
	fmt.Println("seed: development dataset rebuilt")
}

func seed(ctx context.Context, conn *pgx.Conn) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`TRUNCATE legs, transactions, categories, sources, bills, budgets,
		         pending_actions, otp_codes CASCADE`,
	); err != nil {
		return fmt.Errorf("truncate: %w", err)
	}

	categories, err := seedCategories(ctx, tx)
	if err != nil {
		return err
	}
	sources, err := seedSources(ctx, tx)
	if err != nil {
		return err
	}
	if err := seedTransactions(ctx, tx, sources, categories); err != nil {
		return err
	}
	if err := seedBillsAndBudgets(ctx, tx, categories); err != nil {
		return err
	}
	if err := seedPendingAction(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func seedCategories(ctx context.Context, tx pgx.Tx) (map[string]string, error) {
	roots := []struct {
		name      string
		essential bool
	}{
		{"Makanan & Minuman", true},
		{"Transportasi", true},
		{"Tagihan", true},
		{"Belanja", false},
		{"Hiburan", false},
		{"Penghasilan", false},
	}
	ids := map[string]string{}
	for _, root := range roots {
		var id string
		if err := tx.QueryRow(ctx,
			`INSERT INTO categories (name, essential) VALUES ($1, $2) RETURNING id`,
			root.name, root.essential,
		).Scan(&id); err != nil {
			return nil, fmt.Errorf("insert category %s: %w", root.name, err)
		}
		ids[root.name] = id
	}
	for _, child := range []string{"Listrik", "Internet"} {
		var id string
		if err := tx.QueryRow(ctx,
			`INSERT INTO categories (name, parent_id, essential) VALUES ($1, $2, true) RETURNING id`,
			child, ids["Tagihan"],
		).Scan(&id); err != nil {
			return nil, fmt.Errorf("insert category %s: %w", child, err)
		}
		ids[child] = id
	}
	return ids, nil
}

func seedSources(ctx context.Context, tx pgx.Tx) (map[string]string, error) {
	specs := []sourceSpec{
		{"BCA", "bank", false, 8_250_000},
		{"Cash", "cash", false, 750_000},
		{"GoPay", "ewallet", false, 350_000},
		{"SPayLater", "custom", true, -1_200_000},
	}
	ids := map[string]string{}
	opened := jakartaDay(30)
	for _, spec := range specs {
		var id string
		if err := tx.QueryRow(ctx,
			`INSERT INTO sources (name, type, is_debt) VALUES ($1, $2, $3) RETURNING id`,
			spec.name, spec.kind, spec.isDebt,
		).Scan(&id); err != nil {
			return nil, fmt.Errorf("insert source %s: %w", spec.name, err)
		}
		ids[spec.name] = id

		// Opening balance is an adjustment: source leg + system sink,
		// exactly what POST /sources/{id}/adjustments writes.
		note := "Saldo awal"
		txID, err := insertTransaction(ctx, tx, transactionRow{
			occurredAt: opened,
			kind:       "adjustment",
			note:       &note,
		})
		if err != nil {
			return nil, err
		}
		if err := insertLegs(ctx, tx, txID,
			leg{sourceID: &id, amount: spec.opening},
			leg{amount: -spec.opening},
		); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

func seedTransactions(ctx context.Context, tx pgx.Tx, sources, categories map[string]string) error {
	specs := []txSpec{
		{kind: "income", amount: 9_500_000, source: "BCA", category: "Penghasilan", merchant: "Gaji", daysAgo: 13},
		{kind: "expense", amount: 156_000, source: "BCA", category: "Belanja", merchant: "Shopee", daysAgo: 9, needsReview: true, confidence: 0.62},
		{kind: "expense", amount: 25_000, source: "GoPay", category: "Makanan & Minuman", merchant: "Kopi Kenangan", daysAgo: 6},
		{kind: "expense", amount: 32_000, source: "GoPay", category: "Transportasi", merchant: "Gojek", daysAgo: 5},
		{kind: "expense", amount: 30_000, source: "Cash", category: "Makanan & Minuman", merchant: "Warung Bu Sri", daysAgo: 5},
		{kind: "expense", amount: 54_000, source: "BCA", category: "Hiburan", merchant: "Netflix", daysAgo: 3},
		{kind: "expense", amount: 67_500, source: "Cash", category: "Makanan & Minuman", merchant: "Padang Sederhana", daysAgo: 2},
		{kind: "expense", amount: 89_000, source: "SPayLater", category: "Belanja", merchant: "Tokopedia", daysAgo: 1, needsReview: true, confidence: 0.48},
		{kind: "transfer", amount: 200_000, source: "BCA", destination: "GoPay", merchant: "Top up", daysAgo: 1},
	}

	for _, spec := range specs {
		row := transactionRow{
			occurredAt: jakartaDay(spec.daysAgo),
			kind:       spec.kind,
			merchant:   ptr(spec.merchant),
		}
		if spec.kind != "transfer" {
			categoryID := categories[spec.category]
			row.categoryID = &categoryID
			source := "rules"
			if spec.needsReview {
				source = "llm"
				row.reviewStatus = "needs_review"
				row.confidence = &spec.confidence
			}
			row.categorySource = &source
		}

		txID, err := insertTransaction(ctx, tx, row)
		if err != nil {
			return err
		}

		sourceID := sources[spec.source]
		switch spec.kind {
		case "expense":
			categoryID := categories[spec.category]
			err = insertLegs(ctx, tx, txID,
				leg{sourceID: &sourceID, amount: -spec.amount},
				leg{categoryID: &categoryID, amount: spec.amount},
			)
		case "income":
			categoryID := categories[spec.category]
			err = insertLegs(ctx, tx, txID,
				leg{sourceID: &sourceID, amount: spec.amount},
				leg{categoryID: &categoryID, amount: -spec.amount},
			)
		case "transfer":
			destinationID := sources[spec.destination]
			err = insertLegs(ctx, tx, txID,
				leg{sourceID: &sourceID, amount: -spec.amount},
				leg{sourceID: &destinationID, amount: spec.amount},
			)
		default:
			err = fmt.Errorf("unknown seed transaction type %q", spec.kind)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func seedBillsAndBudgets(ctx context.Context, tx pgx.Tx, categories map[string]string) error {
	bills := []struct {
		name     string
		min, max int64
		dueInDay int
		category string
	}{
		{"Listrik PLN", 350_000, 550_000, 1, "Listrik"},
		{"Internet IndiHome", 300_000, 350_000, 6, "Internet"},
		{"Sewa kos", 1_800_000, 1_800_000, 16, "Tagihan"},
	}
	for _, bill := range bills {
		categoryID := categories[bill.category]
		if _, err := tx.Exec(ctx,
			`INSERT INTO bills (name, amount_min_minor, amount_max_minor, cadence, next_due_date, category_id)
			 VALUES ($1, $2, $3, 'monthly', $4, $5)`,
			bill.name, bill.min, bill.max, jakartaDay(-bill.dueInDay).Format("2006-01-02"), categoryID,
		); err != nil {
			return fmt.Errorf("insert bill %s: %w", bill.name, err)
		}
	}

	period := time.Now().In(jakarta).Format("2006-01")
	budgets := []struct {
		category string
		amount   int64
	}{
		{"Makanan & Minuman", 220_000},
		{"Hiburan", 50_000},
		{"Transportasi", 400_000},
	}
	for _, budget := range budgets {
		if _, err := tx.Exec(ctx,
			`INSERT INTO budgets (category_id, period, amount_minor) VALUES ($1, $2, $3)`,
			categories[budget.category], period, budget.amount,
		); err != nil {
			return fmt.Errorf("insert budget %s: %w", budget.category, err)
		}
	}
	return nil
}

func seedPendingAction(ctx context.Context, tx pgx.Tx) error {
	params := fmt.Sprintf(
		`{"type":"expense","amount_minor":75000,"source":"BCA","category":"Belanja","merchant":"Tokopedia","occurred_date":%q}`,
		jakartaDay(1).Format("2006-01-02"),
	)
	if _, err := tx.Exec(ctx,
		`INSERT INTO pending_actions (kind, params, summary) VALUES ('create_transaction', $1::jsonb, $2)`,
		params, "Catat belanja Tokopedia Rp75.000?",
	); err != nil {
		return fmt.Errorf("insert pending action: %w", err)
	}
	return nil
}

// ------------------------------------------------------------ helpers

type transactionRow struct {
	occurredAt     time.Time
	kind           string
	merchant       *string
	note           *string
	categoryID     *string
	categorySource *string
	confidence     *float64
	reviewStatus   string
}

type leg struct {
	sourceID   *string
	categoryID *string
	amount     int64
}

func insertTransaction(ctx context.Context, tx pgx.Tx, row transactionRow) (string, error) {
	status := row.reviewStatus
	if status == "" {
		status = "clean"
	}
	var id string
	err := tx.QueryRow(ctx,
		`INSERT INTO transactions
		   (occurred_at, origin, type, merchant, note, category_id,
		    category_source, category_confidence, review_status)
		 VALUES ($1, 'dashboard_manual', $2, $3, $4, $5, $6, $7, $8)
		 RETURNING id`,
		row.occurredAt, row.kind, row.merchant, row.note, row.categoryID,
		row.categorySource, row.confidence, status,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("insert transaction: %w", err)
	}
	return id, nil
}

func insertLegs(ctx context.Context, tx pgx.Tx, transactionID string, legs ...leg) error {
	for _, l := range legs {
		if _, err := tx.Exec(ctx,
			`INSERT INTO legs (transaction_id, source_id, category_id, signed_amount)
			 VALUES ($1, $2, $3, $4)`,
			transactionID, l.sourceID, l.categoryID, l.amount,
		); err != nil {
			return fmt.Errorf("insert leg: %w", err)
		}
	}
	return nil
}

// jakartaDay returns noon Jakarta `daysAgo` days back — noon keeps the
// instant on the intended day in every timezone a client may use.
func jakartaDay(daysAgo int) time.Time {
	now := time.Now().In(jakarta).AddDate(0, 0, -daysAgo)
	return time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, jakarta)
}

func ptr(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
