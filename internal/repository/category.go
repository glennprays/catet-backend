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

// CategoryRepository persists the category hierarchy (flat rows; the
// tree is assembled in services/clients).
type CategoryRepository struct {
	db *pgxpool.Pool
}

func NewCategoryRepository(db *pgxpool.Pool) *CategoryRepository {
	return &CategoryRepository{db: db}
}

const categoryColumns = `id, name, parent_id, icon, essential, archived_at, created_at`

func scanCategory(row pgx.Row) (*entity.Category, error) {
	var c entity.Category
	err := row.Scan(&c.ID, &c.Name, &c.ParentID, &c.Icon, &c.Essential, &c.ArchivedAt, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (r *CategoryRepository) Create(ctx context.Context, c *entity.Category) (*entity.Category, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, err
	}
	return scanCategory(r.db.QueryRow(ctx,
		`INSERT INTO categories (id, name, parent_id, icon, essential, user_id) VALUES ($1,$2,$3,$4,$5,$6)
		 RETURNING `+categoryColumns,
		c.ID, c.Name, c.ParentID, c.Icon, c.Essential, uid,
	))
}

func (r *CategoryRepository) Get(ctx context.Context, id uuid.UUID) (*entity.Category, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, err
	}
	return scanCategory(r.db.QueryRow(ctx,
		`SELECT `+categoryColumns+` FROM categories WHERE id = $1 AND user_id = $2`, id, uid,
	))
}

func (r *CategoryRepository) List(ctx context.Context, includeArchived bool) ([]entity.Category, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, err
	}
	where := ` WHERE user_id = $1`
	if !includeArchived {
		where += ` AND archived_at IS NULL`
	}
	rows, err := r.db.Query(ctx,
		`SELECT `+categoryColumns+` FROM categories`+where+` ORDER BY name ASC`,
		uid,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []entity.Category{}
	for rows.Next() {
		var c entity.Category
		if err := rows.Scan(&c.ID, &c.Name, &c.ParentID, &c.Icon, &c.Essential, &c.ArchivedAt, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Update applies the provided subset; nil = unchanged. parentID nil =
// keep, non-nil pointer to nil = clear (parent_id NULL), non-nil inner
// = set.
func (r *CategoryRepository) Update(ctx context.Context, id uuid.UUID, name *string, parentID **uuid.UUID, icon *string, essential *bool) (*entity.Category, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, err
	}
	parentProvided := parentID != nil
	var parentValue *uuid.UUID
	if parentID != nil {
		parentValue = *parentID
	}
	return scanCategory(r.db.QueryRow(ctx,
		`UPDATE categories SET
			name = COALESCE($2, name),
			parent_id = CASE WHEN $3 THEN $4::uuid ELSE parent_id END,
			icon = COALESCE($5, icon),
			essential = COALESCE($6, essential)
		 WHERE id = $1 AND user_id = $7
		 RETURNING `+categoryColumns,
		id, name, parentProvided, parentValue, icon, essential, uid,
	))
}

// Subtree returns the category and every descendant, root first, live or
// archived. The depth cap is a belt against a cycle that slipped past
// guardCycle; it never fires on a well-formed tree.
func (r *CategoryRepository) Subtree(ctx context.Context, id uuid.UUID) ([]uuid.UUID, error) {
	uid, err := scope.UserID(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Query(ctx,
		`WITH RECURSIVE tree AS (
			SELECT id, 0 AS depth FROM categories WHERE id = $1 AND user_id = $2
			UNION ALL
			SELECT c.id, t.depth + 1 FROM categories c
			JOIN tree t ON c.parent_id = t.id
			WHERE c.user_id = $2 AND t.depth < 32
		)
		SELECT id FROM tree ORDER BY depth, id`,
		id, uid,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []uuid.UUID{}
	for rows.Next() {
		var got uuid.UUID
		if err := rows.Scan(&got); err != nil {
			return nil, err
		}
		out = append(out, got)
	}
	return out, rows.Err()
}

// ArchiveMany soft-deletes every listed live category of the caller in one
// statement and reports how many rows changed (0 = all were already
// archived or none were theirs).
func (r *CategoryRepository) ArchiveMany(ctx context.Context, ids []uuid.UUID) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	uid, err := scope.UserID(ctx)
	if err != nil {
		return 0, err
	}
	tag, err := r.db.Exec(ctx,
		`UPDATE categories SET archived_at = now() WHERE id = ANY($1) AND user_id = $2 AND archived_at IS NULL`,
		ids, uid,
	)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// StopBudgetsFrom ends every budget of the listed categories at the start of
// month: segments starting on or after it are deleted, one straddling it is
// closed at it. Returns how many rows changed.
func (r *CategoryRepository) StopBudgetsFrom(ctx context.Context, ids []uuid.UUID, month time.Time) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	uid, err := scope.UserID(ctx)
	if err != nil {
		return 0, err
	}
	start := month.Format("2006-01-02")
	dropped, err := r.db.Exec(ctx,
		`DELETE FROM budgets WHERE category_id = ANY($1) AND user_id = $3 AND starts_on >= $2`,
		ids, start, uid,
	)
	if err != nil {
		return 0, err
	}
	closed, err := r.db.Exec(ctx,
		`UPDATE budgets SET ends_before = $2
		 WHERE category_id = ANY($1) AND user_id = $3
		   AND starts_on < $2 AND (ends_before IS NULL OR ends_before > $2)`,
		ids, start, uid,
	)
	if err != nil {
		return 0, err
	}
	return dropped.RowsAffected() + closed.RowsAffected(), nil
}
