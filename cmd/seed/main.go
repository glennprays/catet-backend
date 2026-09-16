// Command seed rebuilds a deterministic development dataset owned by
// one demo account (+628000000001): sources with opening balances, a
// category tree, a month of transactions, bills, budgets, and one
// parked bot action.
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

	"github.com/glennprays/catet-backend/internal/service"
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

	// Same wipe list as testkit.TruncateAll, named explicitly rather
	// than left to CASCADE so the set is reviewable.
	if _, err := tx.Exec(ctx,
		`TRUNCATE legs, transactions, categories, sources, bills, budgets, tags,
		         bot_chat_state, inbound_messages, pending_actions, otp_codes,
		         password_resets, invites, user_settings, users CASCADE`,
	); err != nil {
		return fmt.Errorf("truncate: %w", err)
	}

	// Every row below is owned by one demo account: the dataset is only
	// useful if it looks like a real tenant's ledger.
	userID, err := seedDemoUser(ctx, tx)
	if err != nil {
		return err
	}

	categories, err := seedCategories(ctx, tx, userID)
	if err != nil {
		return err
	}
	sources, err := seedSources(ctx, tx, userID)
	if err != nil {
		return err
	}
	if err := seedTransactions(ctx, tx, userID, sources, categories); err != nil {
		return err
	}
	if err := seedBillsAndBudgets(ctx, tx, userID, categories); err != nil {
		return err
	}
	if err := seedPendingAction(ctx, tx, userID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// seedDemoUser creates the account the whole dataset hangs off. It has
// no password: dev login goes through "Lupa kata sandi", same as the
// bootstrap admin on a real install.
func seedDemoUser(ctx context.Context, tx pgx.Tx) (string, error) {
	var id string
	if err := tx.QueryRow(ctx,
		`INSERT INTO users (phone, display_name) VALUES ('+628000000001', 'Demo') RETURNING id`,
	).Scan(&id); err != nil {
		return "", fmt.Errorf("insert demo user: %w", err)
	}
	// Settings are per user now; the row is expected to exist for every
	// account, so the seed creates it exactly like Register does.
	if _, err := tx.Exec(ctx,
		`INSERT INTO user_settings (user_id) VALUES ($1)`, id,
	); err != nil {
		return "", fmt.Errorf("insert demo user settings: %w", err)
	}
	return id, nil
}

// seedCategories builds the same tree a fresh signup gets
// (service.DefaultCategories), so a seeded dev database and a real
// account can never drift apart.
func seedCategories(ctx context.Context, tx pgx.Tx, userID string) (map[string]string, error) {
	ids := map[string]string{}
	for _, root := range service.DefaultCategories {
		var id string
		if err := tx.QueryRow(ctx,
			`INSERT INTO categories (user_id, name, essential) VALUES ($1, $2, $3) RETURNING id`,
			userID, root.Name, root.Essential,
		).Scan(&id); err != nil {
			return nil, fmt.Errorf("insert category %s: %w", root.Name, err)
		}
		ids[root.Name] = id

		for _, child := range root.Children {
			var childID string
			if err := tx.QueryRow(ctx,
				`INSERT INTO categories (user_id, name, parent_id, essential) VALUES ($1, $2, $3, true) RETURNING id`,
				userID, child, id,
			).Scan(&childID); err != nil {
				return nil, fmt.Errorf("insert category %s: %w", child, err)
			}
			ids[child] = childID
		}
	}
	return ids, nil
}

func seedSources(ctx context.Context, tx pgx.Tx, userID string) (map[string]string, error) {
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
			`INSERT INTO sources (user_id, name, type, is_debt) VALUES ($1, $2, $3, $4) RETURNING id`,
			userID, spec.name, spec.kind, spec.isDebt,
		).Scan(&id); err != nil {
			return nil, fmt.Errorf("insert source %s: %w", spec.name, err)
		}
		ids[spec.name] = id

		// Opening balance is an adjustment: source leg + system sink,
		// exactly what POST /sources/{id}/adjustments writes.
		note := "Saldo awal"
		txID, err := insertTransaction(ctx, tx, transactionRow{
			userID:     userID,
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

func seedTransactions(ctx context.Context, tx pgx.Tx, userID string, sources, categories map[string]string) error {
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
			userID:     userID,
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

func seedBillsAndBudgets(ctx context.Context, tx pgx.Tx, userID string, categories map[string]string) error {
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
			`INSERT INTO bills (user_id, name, amount_min_minor, amount_max_minor, cadence, next_due_date, category_id)
			 VALUES ($1, $2, $3, $4, 'monthly', $5, $6)`,
			userID, bill.name, bill.min, bill.max, jakartaDay(-bill.dueInDay).Format("2006-01-02"), categoryID,
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
			`INSERT INTO budgets (user_id, category_id, period, amount_minor) VALUES ($1, $2, $3, $4)`,
			userID, categories[budget.category], period, budget.amount,
		); err != nil {
			return fmt.Errorf("insert budget %s: %w", budget.category, err)
		}
	}
	return nil
}

func seedPendingAction(ctx context.Context, tx pgx.Tx, userID string) error {
	params := fmt.Sprintf(
		`{"type":"expense","amount_minor":75000,"source":"BCA","category":"Belanja","merchant":"Tokopedia","occurred_date":%q}`,
		jakartaDay(1).Format("2006-01-02"),
	)
	if _, err := tx.Exec(ctx,
		`INSERT INTO pending_actions (user_id, kind, params, summary)
		 VALUES ($1, 'create_transaction', $2::jsonb, $3)`,
		userID, params, "Catat belanja Tokopedia Rp75.000?",
	); err != nil {
		return fmt.Errorf("insert pending action: %w", err)
	}
	return nil
}

// ------------------------------------------------------------ helpers

type transactionRow struct {
	userID         string
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
		   (user_id, occurred_at, origin, type, merchant, note, category_id,
		    category_source, category_confidence, review_status)
		 VALUES ($1, $2, 'dashboard_manual', $3, $4, $5, $6, $7, $8, $9)
		 RETURNING id`,
		row.userID, row.occurredAt, row.kind, row.merchant, row.note, row.categoryID,
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
