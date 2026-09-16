package migrations

import (
	"errors"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres" // postgres:// driver
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

// NewMigrator opens the embedded migrations against databaseURL. The
// caller owns the returned *migrate.Migrate and must Close it.
//
// This lives in the migrations package rather than internal/ on
// purpose: it is a leaf, so internal/testkit can share it with the
// operator CLI without an import cycle through internal/auth.
func NewMigrator(databaseURL string) (*migrate.Migrate, error) {
	src, err := iofs.New(FS, ".")
	if err != nil {
		return nil, err
	}
	return migrate.NewWithSourceInstance("iofs", src, databaseURL)
}

// MigrateUp applies every pending migration. ErrNoChange means the
// database is already current, which is success.
func MigrateUp(databaseURL string) error {
	m, err := NewMigrator(databaseURL)
	if err != nil {
		return err
	}
	defer m.Close()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}
	return nil
}
