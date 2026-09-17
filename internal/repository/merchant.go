package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/scope"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AliasKey is the one normalisation of a raw merchant phrase: trimmed,
// single-spaced, lower-cased. It must stay byte-identical to the SQL
// expression the 0009 migration seeds aliases with
// (lower(regexp_replace(btrim(x), '\s+', ' ', 'g'))), or remembered
// phrases would stop matching.
func AliasKey(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// MerchantRepository persists canonical merchants and the alias memory
// that maps every raw spelling onto one of them.
type MerchantRepository struct {
	db *pgxpool.Pool
}

func NewMerchantRepository(db *pgxpool.Pool) *MerchantRepository {
	return &MerchantRepository{db: db}
}

const merchantColumns = `id, user_id, name, created_at, updated_at`

func scanMerchant(row pgx.Row) (*entity.Merchant, error) {
	var m entity.Merchant
	err := row.Scan(&m.ID, &m.UserID, &m.Name, &m.CreatedAt, &m.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// MerchantRow is one Settings → Merchants line: the canonical name plus
// how much live expense money has gone through it.
type MerchantRow struct {
	ID         uuid.UUID
	Name       string
	TxCount    int64
	TotalMinor int64
	LastUsedAt *time.Time
}

// Get loads one of the caller's merchants; nil when it is not theirs.
func (r *MerchantRepository) Get(ctx context.Context, id uuid.UUID) (*entity.Merchant, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, err
	}
	return scanMerchant(r.db.QueryRow(ctx,
		`SELECT `+merchantColumns+` FROM merchants WHERE id = $1 AND user_id = $2`, id, uid,
	))
}

// FindByAlias is the alias memory: the merchant this exact raw phrase
// already resolved to. nil when the phrase is new.
func (r *MerchantRepository) FindByAlias(ctx context.Context, key string) (*entity.Merchant, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, err
	}
	return scanMerchant(r.db.QueryRow(ctx,
		`SELECT m.id, m.user_id, m.name, m.created_at, m.updated_at
		 FROM merchant_aliases a
		 JOIN merchants m ON m.id = a.merchant_id
		 WHERE a.user_id = $1 AND a.alias_key = $2`,
		uid, key,
	))
}

// FindByName matches the canonical name itself, case-insensitively.
func (r *MerchantRepository) FindByName(ctx context.Context, name string) (*entity.Merchant, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, err
	}
	return scanMerchant(r.db.QueryRow(ctx,
		`SELECT `+merchantColumns+` FROM merchants
		 WHERE user_id = $1 AND lower(name) = lower($2)`,
		uid, name,
	))
}

// Create inserts a merchant, or returns the existing row when another
// writer won the race on (user_id, lower(name)).
func (r *MerchantRepository) Create(ctx context.Context, name string) (*entity.Merchant, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, err
	}
	m, err := scanMerchant(r.db.QueryRow(ctx,
		`INSERT INTO merchants (user_id, name) VALUES ($1,$2) RETURNING `+merchantColumns,
		uid, name,
	))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return r.FindByName(ctx, name)
		}
		return nil, err
	}
	return m, nil
}

// PutAlias remembers (or re-points) one raw phrase. The key is the
// caller's normalised alias key, never the raw text.
func (r *MerchantRepository) PutAlias(ctx context.Context, key string, merchantID uuid.UUID, source string) error {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return err
	}
	_, err = r.db.Exec(ctx,
		`INSERT INTO merchant_aliases (user_id, alias_key, merchant_id, source)
		 VALUES ($1,$2,$3,$4)
		 ON CONFLICT (user_id, alias_key)
		 DO UPDATE SET merchant_id = EXCLUDED.merchant_id, source = EXCLUDED.source`,
		uid, key, merchantID, source,
	)
	return err
}

// Candidates is the whitelist handed to the model: the merchants this
// user actually uses, most recently spent-at first.
func (r *MerchantRepository) Candidates(ctx context.Context, limit int) ([]entity.Merchant, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 300
	}
	rows, err := r.db.Query(ctx,
		`SELECT m.id, m.user_id, m.name, m.created_at, m.updated_at
		 FROM merchants m
		 LEFT JOIN LATERAL (
		   SELECT MAX(t.occurred_at) AS last_used
		   FROM transactions t
		   WHERE t.merchant_id = m.id AND t.voided_at IS NULL
		 ) u ON true
		 WHERE m.user_id = $1
		 ORDER BY u.last_used DESC NULLS LAST, m.name ASC
		 LIMIT $2`,
		uid, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []entity.Merchant{}
	for rows.Next() {
		var m entity.Merchant
		if err := rows.Scan(&m.ID, &m.UserID, &m.Name, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// List is the Settings page: every merchant with its live expense
// volume, biggest spend first.
func (r *MerchantRepository) List(ctx context.Context) ([]MerchantRow, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Query(ctx,
		`SELECT m.id, m.name,
		        COALESCE(s.tx_count, 0), COALESCE(s.total_minor, 0), s.last_used
		 FROM merchants m
		 LEFT JOIN LATERAL (
		   SELECT COUNT(DISTINCT t.id) AS tx_count,
		          COALESCE(SUM(-l.signed_amount), 0) AS total_minor,
		          MAX(t.occurred_at) AS last_used
		   FROM transactions t
		   JOIN legs l ON l.transaction_id = t.id
		     AND l.source_id IS NOT NULL AND l.signed_amount < 0
		   WHERE t.merchant_id = m.id AND t.type = 'expense' AND t.voided_at IS NULL
		 ) s ON true
		 WHERE m.user_id = $1
		 ORDER BY COALESCE(s.total_minor, 0) DESC, m.name ASC`,
		uid,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []MerchantRow{}
	for rows.Next() {
		var m MerchantRow
		if err := rows.Scan(&m.ID, &m.Name, &m.TxCount, &m.TotalMinor, &m.LastUsedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ErrMerchantNameTaken is the unique-name collision the service turns
// into a 409 ("rename to a name that exists" is a merge, not a rename).
var ErrMerchantNameTaken = errors.New("merchant name already exists")

// Rename renames the merchant and rewrites the denormalised display text
// on every row pointing at it, so the ledger never shows the old name.
func (r *MerchantRepository) Rename(ctx context.Context, id uuid.UUID, name string) (*entity.Merchant, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, err
	}
	pgTx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = pgTx.Rollback(ctx) }()

	m, err := scanMerchant(pgTx.QueryRow(ctx,
		`UPDATE merchants SET name = $2, updated_at = now()
		 WHERE id = $1 AND user_id = $3 RETURNING `+merchantColumns,
		id, name, uid,
	))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, ErrMerchantNameTaken
		}
		return nil, err
	}
	if m == nil {
		return nil, nil
	}
	if _, err := pgTx.Exec(ctx,
		`UPDATE transactions SET merchant = $2 WHERE merchant_id = $1 AND user_id = $3`,
		id, name, uid,
	); err != nil {
		return nil, fmt.Errorf("rewrite merchant text: %w", err)
	}
	if err := pgTx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return m, nil
}

// Merge folds `from` into `into`: aliases, rows and display text move,
// the old name becomes an alias of the survivor, and the source row is
// deleted. Both ids must belong to the caller.
func (r *MerchantRepository) Merge(ctx context.Context, fromID, intoID uuid.UUID) error {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return err
	}
	pgTx, err := r.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = pgTx.Rollback(ctx) }()

	var fromName, intoName string
	if err := pgTx.QueryRow(ctx,
		`SELECT name FROM merchants WHERE id = $1 AND user_id = $2`, fromID, uid,
	).Scan(&fromName); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return pgx.ErrNoRows
		}
		return err
	}
	if err := pgTx.QueryRow(ctx,
		`SELECT name FROM merchants WHERE id = $1 AND user_id = $2`, intoID, uid,
	).Scan(&intoName); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return pgx.ErrNoRows
		}
		return err
	}

	// Aliases are keyed per raw phrase, so re-pointing them can never
	// collide: the key stays, only its target moves.
	if _, err := pgTx.Exec(ctx,
		`UPDATE merchant_aliases SET merchant_id = $2, source = 'user'
		 WHERE user_id = $1 AND merchant_id = $3`,
		uid, intoID, fromID,
	); err != nil {
		return fmt.Errorf("repoint aliases: %w", err)
	}
	if _, err := pgTx.Exec(ctx,
		`UPDATE transactions SET merchant_id = $2, merchant = $3
		 WHERE user_id = $1 AND merchant_id = $4`,
		uid, intoID, intoName, fromID,
	); err != nil {
		return fmt.Errorf("repoint transactions: %w", err)
	}
	// The name the user merged away stays findable: typing it again
	// lands on the survivor instead of recreating the duplicate.
	if _, err := pgTx.Exec(ctx,
		`INSERT INTO merchant_aliases (user_id, alias_key, merchant_id, source)
		 VALUES ($1,$2,$3,'user')
		 ON CONFLICT (user_id, alias_key)
		 DO UPDATE SET merchant_id = EXCLUDED.merchant_id, source = EXCLUDED.source`,
		uid, AliasKey(fromName), intoID,
	); err != nil {
		return fmt.Errorf("remember merged name: %w", err)
	}
	if _, err := pgTx.Exec(ctx,
		`DELETE FROM merchants WHERE id = $1 AND user_id = $2`, fromID, uid,
	); err != nil {
		return fmt.Errorf("delete merged merchant: %w", err)
	}
	return pgTx.Commit(ctx)
}
