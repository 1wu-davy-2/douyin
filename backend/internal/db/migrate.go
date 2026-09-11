package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrations holds one SQL script per schema version; migrations[i] upgrades
// user_version i -> i+1.
var migrations []string

func init() {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		panic(fmt.Sprintf("db: read embedded migrations: %v", err))
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		raw, err := migrationsFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			panic(fmt.Sprintf("db: read embedded migration %s: %v", e.Name(), err))
		}
		migrations = append(migrations, string(raw))
	}
}

// Migrate applies all pending embedded migrations, gated by PRAGMA
// user_version. It is idempotent: calling it on an up-to-date or already
// migrated database is a no-op.
func Migrate(handle *sql.DB) error {
	var version int
	if err := handle.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("db: read user_version: %w", err)
	}
	for v := version; v < len(migrations); v++ {
		target := v + 1
		script := migrations[v]
		err := WithTx(context.Background(), handle, func(tx *sql.Tx) error {
			if _, err := tx.Exec(script); err != nil {
				return fmt.Errorf("db: apply migration v%d: %w", target, err)
			}
			// user_version writes are transactional in SQLite; setting it in
			// the same tx as the DDL makes the migration atomic.
			if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", target)); err != nil {
				return fmt.Errorf("db: set user_version v%d: %w", target, err)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}
