package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	handle, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { handle.Close() })
	return handle
}

func TestMigrateIdempotent(t *testing.T) {
	handle := openTestDB(t)

	if err := Migrate(handle); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	if err := Migrate(handle); err != nil {
		t.Fatalf("second migrate (must be a no-op): %v", err)
	}

	var version int
	if err := handle.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != 1 {
		t.Fatalf("user_version = %d, want 1", version)
	}
}

func TestMigrateSchema(t *testing.T) {
	handle := openTestDB(t)
	if err := Migrate(handle); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	wantTables := []string{
		"creators", "collections", "works", "assets", "download_jobs",
		"subscriptions", "scan_runs", "sessions", "settings", "provider_state",
	}
	for _, table := range wantTables {
		var name string
		err := handle.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&name)
		if err != nil {
			t.Errorf("table %s missing: %v", table, err)
		}
	}

	// works uniqueness is per (creator_id, item_id), NOT global.
	ctx := context.Background()
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := handle.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("exec %s: %v", q, err)
		}
	}
	mustExec(`INSERT INTO creators(id, sec_uid, created_at) VALUES (1, 'secA', '2026-01-01T00:00:00Z')`)
	mustExec(`INSERT INTO creators(id, sec_uid, created_at) VALUES (2, 'secB', '2026-01-01T00:00:00Z')`)
	mustExec(`INSERT INTO works(id, creator_id, item_id, created_at, updated_at) VALUES (1, 1, 'itemX', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)
	// Same item_id under a different creator must be accepted.
	mustExec(`INSERT INTO works(id, creator_id, item_id, created_at, updated_at) VALUES (2, 2, 'itemX', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)
	if _, err := handle.ExecContext(ctx,
		`INSERT INTO works(id, creator_id, item_id, created_at, updated_at) VALUES (3, 1, 'itemX', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err == nil {
		t.Fatal("duplicate (creator_id, item_id) must be rejected")
	}

	// "trigger" is a keyword — the scan_runs column must still be usable.
	mustExec(`INSERT INTO scan_runs(id, creator_id, "trigger", status, started_at) VALUES (1, 1, 'manual', 'running', '2026-01-01T00:00:00Z')`)
	var trigger string
	if err := handle.QueryRow(`SELECT "trigger" FROM scan_runs WHERE id=1`).Scan(&trigger); err != nil {
		t.Fatalf("read scan_runs.trigger: %v", err)
	}
	if trigger != "manual" {
		t.Fatalf("scan_runs.trigger = %q, want manual", trigger)
	}

	// download_jobs indexes exist.
	rows, err := handle.Query(`SELECT name FROM sqlite_master WHERE type='index' AND tbl_name='download_jobs'`)
	if err != nil {
		t.Fatalf("list indexes: %v", err)
	}
	defer rows.Close()
	indexes := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan index: %v", err)
		}
		indexes[name] = true
	}
	for _, want := range []string{"idx_download_jobs_status", "idx_download_jobs_work"} {
		if !indexes[want] {
			t.Errorf("missing index %s (got %v)", want, indexes)
		}
	}
}

func TestWithTxRollback(t *testing.T) {
	handle := openTestDB(t)
	if err := Migrate(handle); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	boom := context.Canceled
	err := WithTx(context.Background(), handle, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO settings(key, value) VALUES ('k', 'v')`); err != nil {
			return err
		}
		return boom
	})
	if err == nil {
		t.Fatal("WithTx must propagate fn error")
	}

	var count int
	if err := handle.QueryRow(`SELECT COUNT(*) FROM settings`).Scan(&count); err != nil {
		t.Fatalf("count settings: %v", err)
	}
	if count != 0 {
		t.Fatalf("settings rows = %d, want 0 (tx must roll back)", count)
	}

	// And the happy path commits.
	if err := WithTx(context.Background(), handle, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO settings(key, value) VALUES ('k', 'v')`)
		return err
	}); err != nil {
		t.Fatalf("committing tx: %v", err)
	}
}

func TestVacuum(t *testing.T) {
	handle := openTestDB(t)
	if err := Migrate(handle); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := Vacuum(handle); err != nil {
		t.Fatalf("vacuum: %v", err)
	}
}
