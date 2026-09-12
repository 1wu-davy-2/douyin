package db

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
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

// goMigrations holds the Go-side data migrations, keyed by the TARGET schema
// version. They run inside the same transaction as their SQL script (after
// it, before user_version is bumped), so a failure rolls the whole version
// step back atomically. The data dir is passed in because data migrations may
// need it to rewrite stored paths (v3 absolutizes assets.path against it).
var goMigrations = map[int]func(tx *sql.Tx, dataDir string) error{
	3: absolutizeAssetPaths,
}

// Migrate applies all pending embedded migrations, gated by PRAGMA
// user_version. It is idempotent: calling it on an up-to-date or already
// migrated database is a no-op. dataDir is the tool's data directory; it is
// required by Go-side data migrations (v3 absolutizes stored asset paths
// against it) and may be empty only when no pending migration needs it.
func Migrate(handle *sql.DB, dataDir string) error {
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
			if step, ok := goMigrations[target]; ok {
				if err := step(tx, dataDir); err != nil {
					return fmt.Errorf("db: go migration v%d: %w", target, err)
				}
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

// absolutizeAssetPaths is the Go side of migration v3 (contract v1.3):
// historical assets.path values are relative to the data dir with forward
// slashes ("downloads/{creator}/singles/x.mp4"); from v1.3 on every
// assets.path is absolute. Each relative path is prefixed with the data dir,
// normalized to an absolute forward-slash form with a trailing slash, so the
// stored value reads "E:/data/downloads/...".
//
// Idempotent by construction: rows whose path already looks absolute (a
// drive-letter form "X:..." or a UNC "\\...") do not match the WHERE clause
// and are left untouched.
func absolutizeAssetPaths(tx *sql.Tx, dataDir string) error {
	if strings.TrimSpace(dataDir) == "" {
		return errors.New(`assets.path absolutization requires the data dir (DY_DATA_DIR)`)
	}
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		abs = dataDir
	}
	prefix := filepath.ToSlash(abs)
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	res, err := tx.Exec(`
		UPDATE assets SET path = ? || path
		WHERE path NOT LIKE '_:%' AND path NOT LIKE '\\%'`, prefix)
	if err != nil {
		return fmt.Errorf("rewrite relative asset paths: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		// Logged without the package prefix noise; migrations run once at boot.
		fmt.Printf("db: migration v3: absolutized %d asset path(s) under %s\n", n, prefix)
	}
	return nil
}
