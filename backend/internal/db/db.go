// Package db owns the SQLite database: connection setup (WAL, foreign keys,
// busy timeout), embedded schema migrations gated by PRAGMA user_version, and
// small transaction helpers shared by every store.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (no CGO)
)

// Open opens (creating if necessary) the SQLite database at path.
//
// The connection DSN enables the pragmas the project relies on:
// journal_mode=WAL, foreign_keys=1 and busy_timeout=5000ms.
func Open(path string) (*sql.DB, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("db: create directory %s: %w", dir, err)
		}
	}
	handle, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("db: open %s: %w", path, err)
	}
	if err := handle.Ping(); err != nil {
		handle.Close()
		return nil, fmt.Errorf("db: ping %s: %w", path, err)
	}
	return handle, nil
}

// dsn builds the modernc.org/sqlite DSN with the project pragmas.
//
// Regular paths are converted to an absolute file URI so that SQLite's own URI
// parser (modernc passes SQLITE_OPEN_URI for file: DSNs) resolves the same file
// regardless of the process cwd; _pragma values are consumed by the driver
// itself (url.ParseQuery) and ignored by SQLite's URI parser.
func dsn(path string) string {
	const pragmas = "_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	if strings.HasPrefix(path, "file:") || path == ":memory:" {
		return path + "?" + pragmas
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	p := filepath.ToSlash(path)
	if runtime.GOOS == "windows" && !strings.HasPrefix(p, "/") {
		// file:///C:/... — the extra leading slash is the empty URI authority.
		p = "/" + p
	}
	u := url.URL{Scheme: "file", Path: p}
	return u.String() + "?" + pragmas
}

// Close closes the database handle, tolerating nil.
func Close(handle *sql.DB) error {
	if handle == nil {
		return nil
	}
	return handle.Close()
}

// WithTx runs fn inside a transaction, committing on success and rolling back
// on error (the original error is preserved; a failed rollback is only
// returned when fn itself succeeded up to that point).
func WithTx(ctx context.Context, handle *sql.DB, fn func(tx *sql.Tx) error) (err error) {
	tx, err := handle.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("db: begin tx: %w", err)
	}
	defer func() {
		if rerr := tx.Rollback(); rerr != nil && !errors.Is(rerr, sql.ErrTxDone) && err == nil {
			err = fmt.Errorf("db: rollback: %w", rerr)
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("db: commit: %w", err)
	}
	return nil
}

// Vacuum runs VACUUM (must not be called while a transaction is open).
func Vacuum(handle *sql.DB) error {
	_, err := handle.Exec("VACUUM")
	if err != nil {
		return fmt.Errorf("db: vacuum: %w", err)
	}
	return nil
}
