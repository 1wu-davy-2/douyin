package db

import (
	"context"
	"database/sql"
	"fmt"
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

	if err := Migrate(handle, t.TempDir()); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	if err := Migrate(handle, t.TempDir()); err != nil {
		t.Fatalf("second migrate (must be a no-op): %v", err)
	}

	var version int
	if err := handle.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != 5 {
		t.Fatalf("user_version = %d, want 5", version)
	}
}

// Migration 0005 (contract v1.4): creators.alias and creators.group_name
// exist, are NULL for pre-existing rows and round-trip arbitrary text.
func TestMigrateCreatorAliasGroup(t *testing.T) {
	handle := openTestDB(t)
	if err := Migrate(handle, t.TempDir()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	ctx := context.Background()
	if _, err := handle.ExecContext(ctx,
		`INSERT INTO creators(id, sec_uid, created_at) VALUES (1, 'secA', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	var alias, groupName sql.NullString
	if err := handle.QueryRowContext(ctx,
		`SELECT alias, group_name FROM creators WHERE id = 1`).Scan(&alias, &groupName); err != nil {
		t.Fatalf("select alias/group_name: %v", err)
	}
	if alias.Valid || groupName.Valid {
		t.Fatalf("alias/group_name = %v/%v, want NULL/NULL", alias, groupName)
	}
	if _, err := handle.ExecContext(ctx,
		`UPDATE creators SET alias = '别名', group_name = '默认分组' WHERE id = 1`); err != nil {
		t.Fatalf("update alias/group_name: %v", err)
	}
	if err := handle.QueryRowContext(ctx,
		`SELECT alias, group_name FROM creators WHERE id = 1`).Scan(&alias, &groupName); err != nil {
		t.Fatal(err)
	}
	if !alias.Valid || alias.String != "别名" || !groupName.Valid || groupName.String != "默认分组" {
		t.Fatalf("alias/group_name roundtrip = %v/%v", alias, groupName)
	}
}

// Migration 0002: works.type exists, defaults to 'video' for pre-existing
// rows and accepts 'image' for gallery works.
func TestMigrateWorkType(t *testing.T) {
	handle := openTestDB(t)
	if err := Migrate(handle, t.TempDir()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := Migrate(handle, t.TempDir()); err != nil {
		t.Fatalf("re-migrate (must be a no-op): %v", err)
	}

	ctx := context.Background()
	if _, err := handle.ExecContext(ctx,
		`INSERT INTO creators(id, sec_uid, created_at) VALUES (1, 'secA', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	// Omitted type -> the 'video' default (pre-upgrade rows keep working).
	if _, err := handle.ExecContext(ctx,
		`INSERT INTO works(id, creator_id, item_id, created_at, updated_at)
		 VALUES (1, 1, 'itemV', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	var videoType string
	if err := handle.QueryRow(`SELECT type FROM works WHERE id = 1`).Scan(&videoType); err != nil {
		t.Fatal(err)
	}
	if videoType != "video" {
		t.Fatalf("default type = %q, want video", videoType)
	}
	// Explicit image type is accepted.
	if _, err := handle.ExecContext(ctx,
		`INSERT INTO works(id, creator_id, item_id, type, created_at, updated_at)
		 VALUES (2, 1, 'itemI', 'image', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	var imageType string
	if err := handle.QueryRow(`SELECT type FROM works WHERE id = 2`).Scan(&imageType); err != nil {
		t.Fatal(err)
	}
	if imageType != "image" {
		t.Fatalf("image type = %q, want image", imageType)
	}
}

func TestMigrateSchema(t *testing.T) {
	handle := openTestDB(t)
	if err := Migrate(handle, t.TempDir()); err != nil {
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
	if err := Migrate(handle, t.TempDir()); err != nil {
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
	if err := Migrate(handle, t.TempDir()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := Vacuum(handle); err != nil {
		t.Fatalf("vacuum: %v", err)
	}
}

// ------------------------------------------------------ migration 0003 ----

// seedV2 builds a pre-v3 database by replaying only migrations 0001+0002 —
// the exact shape a v1.2 installation has on disk before the upgrade.
func seedV2(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dir := t.TempDir()
	handle, err := Open(filepath.Join(dir, "v2.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { handle.Close() })
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := WithTx(ctx, handle, func(tx *sql.Tx) error {
			if _, err := tx.Exec(migrations[i]); err != nil {
				return err
			}
			_, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1))
			return err
		}); err != nil {
			t.Fatalf("seed user_version v%d: %v", i+1, err)
		}
	}
	return handle, dir
}

// Migration 0003 Go step: relative assets.path values get the data dir
// prefixed (absolute, forward slashes); already-absolute rows (drive letter
// or UNC) are left untouched, which keeps the step idempotent. The legacy
// junction corpus ("downloads/legacy/...") is absolutized too — it keeps
// resolving through the junction, now as an absolute path.
func TestMigrate0003AbsolutizesRelativeAssetPaths(t *testing.T) {
	handle, dir := seedV2(t)
	ctx := context.Background()

	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := handle.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("exec %s: %v", q, err)
		}
	}
	mustExec(`INSERT INTO creators(id, sec_uid, nickname, created_at) VALUES (1, 'secA', 'A', '2026-01-01T00:00:00Z')`)
	mustExec(`INSERT INTO works(id, creator_id, item_id, created_at, updated_at)
		  VALUES (1, 1, 'item1', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)
	mustExec(`INSERT INTO works(id, creator_id, item_id, created_at, updated_at)
		  VALUES (2, 1, 'item2', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)
	insertAsset := func(workID int64, kind, path string, quality sql.NullString) {
		t.Helper()
		mustExec(`INSERT INTO assets (work_id, kind, path, size_bytes, quality, created_at)
			  VALUES (?, ?, ?, 1, ?, '2026-01-01T00:00:00Z')`, workID, kind, path, quality)
	}
	video1080 := sql.NullString{String: "1080p", Valid: true}
	video720 := sql.NullString{String: "720p", Valid: true}
	insertAsset(1, "video", "downloads/UP主_secsA/singles/a.mp4", video1080)
	insertAsset(1, "metadata", "downloads/UP主_secsA/singles/a.metadata.json", sql.NullString{})
	insertAsset(2, "cover", "downloads/legacy/old/cover.jpg", sql.NullString{}) // legacy junction corpus
	insertAsset(2, "video", "E:/elsewhere/abs.mp4", video1080)                  // absolute: untouched
	insertAsset(2, "video", `\\server/share/unc.mp4`, video720)                 // UNC: untouched

	if err := Migrate(handle, dir); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	prefix := filepath.ToSlash(filepath.Clean(dir)) + "/"
	want := map[string]string{
		"a.mp4":           prefix + "downloads/UP主_secsA/singles/a.mp4",
		"a.metadata.json": prefix + "downloads/UP主_secsA/singles/a.metadata.json",
		"cover.jpg":       prefix + "downloads/legacy/old/cover.jpg",
		"abs.mp4":         "E:/elsewhere/abs.mp4",
		"unc.mp4":         `\\server/share/unc.mp4`,
	}
	assets := map[string]string{}
	rows, err := handle.QueryContext(ctx, `SELECT path FROM assets`)
	if err != nil {
		t.Fatalf("list assets: %v", err)
	}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		assets[filepath.Base(p)] = p
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(assets) != len(want) {
		t.Fatalf("assets = %v, want %d rows", assets, len(want))
	}
	for base, abs := range want {
		if assets[base] != abs {
			t.Errorf("asset %s path = %q, want %q", base, assets[base], abs)
		}
	}

	// creators.download_root exists and defaults to NULL.
	var root any
	if err := handle.QueryRow(`SELECT download_root FROM creators WHERE id = 1`).Scan(&root); err != nil {
		t.Fatalf("read creators.download_root: %v", err)
	}
	if root != nil {
		t.Errorf("download_root default = %v, want NULL", root)
	}

	// Idempotent: re-running changes nothing (already-absolute rows stay).
	if err := Migrate(handle, dir); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	for base, abs := range want {
		var got string
		if err := handle.QueryRow(`SELECT path FROM assets WHERE path = ?`, abs).Scan(&got); err != nil {
			t.Errorf("post-re-migrate asset %s missing: %v", base, err)
		}
	}
}

// The Go step of 0003 needs the data dir; without it the whole version step
// (schema change + data rewrite + user_version) must roll back atomically.
func TestMigrate0003RequiresDataDir(t *testing.T) {
	handle, _ := seedV2(t)

	if err := Migrate(handle, ""); err == nil {
		t.Fatal("migrate without data dir must fail on pending v3")
	}
	var version int
	if err := handle.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 2 {
		t.Fatalf("user_version = %d after failed migrate, want 2 (rolled back)", version)
	}
	// The ALTER TABLE from the rolled-back step must not have stuck.
	if _, err := handle.Exec(`SELECT download_root FROM creators LIMIT 1`); err == nil {
		t.Fatal("creators.download_root exists after a rolled-back migration")
	}
}
