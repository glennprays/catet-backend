package repository

import (
	"context"
	"errors"
	"time"

	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// UserRepository persists accounts. Users are not tenant-owned rows —
// they *are* the tenants — so nothing here reads internal/scope.
type UserRepository struct {
	db *pgxpool.Pool
}

func NewUserRepository(db *pgxpool.Pool) *UserRepository {
	return &UserRepository{db: db}
}

const userColumns = `id, phone, phone_digits, whatsapp_lid, display_name, password_hash,
	                 is_admin, session_epoch, failed_logins, locked_until, deactivated_at,
	                 last_login_at, created_at`

func scanUser(row pgx.Row) (*entity.User, error) {
	var u entity.User
	err := row.Scan(
		&u.ID, &u.Phone, &u.PhoneDigits, &u.WhatsAppLID, &u.DisplayName, &u.PasswordHash,
		&u.IsAdmin, &u.SessionEpoch, &u.FailedLogins, &u.LockedUntil, &u.DeactivatedAt,
		&u.LastLoginAt, &u.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// Create inserts an account and its settings row in one transaction:
// every read of user_settings assumes the row exists.
func (r *UserRepository) Create(ctx context.Context, u *entity.User) (*entity.User, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	created, err := scanUser(tx.QueryRow(ctx,
		`INSERT INTO users (phone, whatsapp_lid, display_name, password_hash, is_admin)
		 VALUES ($1,$2,$3,$4,$5)
		 RETURNING `+userColumns,
		u.Phone, u.WhatsAppLID, u.DisplayName, u.PasswordHash, u.IsAdmin,
	))
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO user_settings (user_id) VALUES ($1)`, created.ID,
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return created, nil
}

func (r *UserRepository) GetByID(ctx context.Context, id uuid.UUID) (*entity.User, error) {
	return scanUser(r.db.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE id = $1`, id))
}

// GetByPhone looks an account up by its E.164 identity; nil when the
// number is not registered.
func (r *UserRepository) GetByPhone(ctx context.Context, phone string) (*entity.User, error) {
	return scanUser(r.db.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE phone = $1`, phone))
}

// FindByWhatsApp resolves an inbound sender. The gateway delivers either
// the phone's digits or an opaque lid, so both columns are candidates.
func (r *UserRepository) FindByWhatsApp(ctx context.Context, digits string) (*entity.User, error) {
	if digits == "" {
		return nil, nil
	}
	return scanUser(r.db.QueryRow(ctx,
		`SELECT `+userColumns+` FROM users WHERE phone_digits = $1 OR whatsapp_lid = $1`, digits,
	))
}

func (r *UserRepository) List(ctx context.Context) ([]entity.User, error) {
	rows, err := r.db.Query(ctx, `SELECT `+userColumns+` FROM users ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []entity.User{}
	for rows.Next() {
		var u entity.User
		if err := rows.Scan(
			&u.ID, &u.Phone, &u.PhoneDigits, &u.WhatsAppLID, &u.DisplayName, &u.PasswordHash,
			&u.IsAdmin, &u.SessionEpoch, &u.FailedLogins, &u.LockedUntil, &u.DeactivatedAt,
			&u.LastLoginAt, &u.CreatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SetPassword stores a new hash, clears the lockout and bumps the
// session epoch: a password change signs every device out.
func (r *UserRepository) SetPassword(ctx context.Context, id uuid.UUID, hash string) error {
	tag, err := r.db.Exec(ctx,
		`UPDATE users SET password_hash = $2, failed_logins = 0, locked_until = NULL,
		        session_epoch = session_epoch + 1
		 WHERE id = $1`,
		id, hash,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// maxLoginFailures is how many consecutive wrong passwords lock an
// account; lockoutWindow is how long it stays locked.
const (
	maxLoginFailures = 5
	lockoutWindow    = 15 * time.Minute
)

// RecordLoginFailure counts one wrong password and locks the account on
// the fifth. It returns the new counter and the lock instant, so the
// service can answer "locked" on the attempt that caused it rather than
// on the next one.
func (r *UserRepository) RecordLoginFailure(ctx context.Context, id uuid.UUID) (int, *time.Time, error) {
	var (
		failed      int
		lockedUntil *time.Time
	)
	err := r.db.QueryRow(ctx,
		`UPDATE users SET
		   failed_logins = CASE WHEN failed_logins + 1 >= $2 THEN 0 ELSE failed_logins + 1 END,
		   locked_until  = CASE WHEN failed_logins + 1 >= $2 THEN now() + make_interval(mins => $3) ELSE locked_until END
		 WHERE id = $1
		 RETURNING failed_logins, locked_until`,
		id, maxLoginFailures, int(lockoutWindow.Minutes()),
	).Scan(&failed, &lockedUntil)
	if err != nil {
		return 0, nil, err
	}
	return failed, lockedUntil, nil
}

// RecordLoginSuccess clears the failure bookkeeping and stamps the login.
func (r *UserRepository) RecordLoginSuccess(ctx context.Context, id uuid.UUID) error {
	_, err := r.db.Exec(ctx,
		`UPDATE users SET failed_logins = 0, locked_until = NULL, last_login_at = now() WHERE id = $1`,
		id,
	)
	return err
}

// SetPhone moves an account to another number (operator CLI only). A nil
// lid clears the stored lid, because the old one belongs to the old
// number.
func (r *UserRepository) SetPhone(ctx context.Context, id uuid.UUID, phone string, lid *string) error {
	tag, err := r.db.Exec(ctx,
		`UPDATE users SET phone = $2, whatsapp_lid = $3 WHERE id = $1`, id, phone, lid,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// SetLID records the opaque WhatsApp identity, learned lazily from the
// gateway the first time we see the account.
func (r *UserRepository) SetLID(ctx context.Context, id uuid.UUID, lid string) error {
	_, err := r.db.Exec(ctx, `UPDATE users SET whatsapp_lid = $2 WHERE id = $1`, id, lid)
	return err
}

// SetDeactivated soft-disables (at != nil) or restores an account.
// Disabling also bumps the epoch, so open sessions die immediately.
func (r *UserRepository) SetDeactivated(ctx context.Context, id uuid.UUID, at *time.Time) error {
	tag, err := r.db.Exec(ctx,
		`UPDATE users SET deactivated_at = $2,
		        session_epoch = CASE WHEN $2::timestamptz IS NULL THEN session_epoch ELSE session_epoch + 1 END
		 WHERE id = $1`,
		id, at,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// BumpEpoch invalidates every session this account has open.
func (r *UserRepository) BumpEpoch(ctx context.Context, id uuid.UUID) error {
	tag, err := r.db.Exec(ctx, `UPDATE users SET session_epoch = session_epoch + 1 WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}
