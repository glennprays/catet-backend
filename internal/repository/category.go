package repository

import (
	"context"
	"errors"

	"github.com/glennprays/catet-backend/internal/domain/entity"
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
	return scanCategory(r.db.QueryRow(ctx,
		`INSERT INTO categories (id, name, parent_id, icon, essential) VALUES ($1,$2,$3,$4,$5)
		 RETURNING `+categoryColumns,
		c.ID, c.Name, c.ParentID, c.Icon, c.Essential,
	))
}

func (r *CategoryRepository) Get(ctx context.Context, id uuid.UUID) (*entity.Category, error) {
	return scanCategory(r.db.QueryRow(ctx,
		`SELECT `+categoryColumns+` FROM categories WHERE id = $1`, id,
	))
}

func (r *CategoryRepository) List(ctx context.Context, includeArchived bool) ([]entity.Category, error) {
	where := ""
	if !includeArchived {
		where = ` WHERE archived_at IS NULL`
	}
	rows, err := r.db.Query(ctx,
		`SELECT `+categoryColumns+` FROM categories`+where+` ORDER BY name ASC`,
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
		 WHERE id = $1
		 RETURNING `+categoryColumns,
		id, name, parentProvided, parentValue, icon, essential,
	))
}

// Archive soft-deletes a category. Returns nil when missing or already
// archived.
func (r *CategoryRepository) Archive(ctx context.Context, id uuid.UUID) (*entity.Category, error) {
	return scanCategory(r.db.QueryRow(ctx,
		`UPDATE categories SET archived_at = now() WHERE id = $1 AND archived_at IS NULL
		 RETURNING `+categoryColumns,
		id,
	))
}

// HasBudgetInPeriod reports whether a budget exists for the category
// in the given YYYY-MM period (archive guard).
func (r *CategoryRepository) HasBudgetInPeriod(ctx context.Context, id uuid.UUID, period string) (bool, error) {
	var exists bool
	err := r.db.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM budgets WHERE category_id = $1 AND period = $2)`,
		id, period,
	).Scan(&exists)
	return exists, err
}
