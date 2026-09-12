package legacy

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// fixture builds a miniature legacy v1 database plus a downloads tree:
//
//	creator 1 (wen) with collection 10 and works 100 (complete, quality 720)
//	and work 101 (video row present but the file is missing on disk).
func fixture(t *testing.T) (legacyDB, downloads, dataDir string) {
	t.Helper()
	dir := t.TempDir()
	legacyDB = filepath.Join(dir, "legacy.db")
	downloads = filepath.Join(dir, "downloads")
	dataDir = filepath.Join(dir, "data")

	h, err := sql.Open("sqlite", legacyDB)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	schema := `
	CREATE TABLE creators (
	  id INTEGER PRIMARY KEY AUTOINCREMENT, sec_user_id TEXT NOT NULL UNIQUE,
	  profile_url TEXT NOT NULL, nickname TEXT NOT NULL DEFAULT '',
	  avatar_url TEXT NOT NULL DEFAULT '', enabled INTEGER NOT NULL DEFAULT 1,
	  last_scanned_at TEXT, last_scan_status TEXT NOT NULL DEFAULT 'never',
	  created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
	  reported_work_count INTEGER NOT NULL DEFAULT 0);
	CREATE TABLE collections (
	  id INTEGER PRIMARY KEY AUTOINCREMENT, creator_id INTEGER NOT NULL REFERENCES creators(id),
	  mix_id TEXT NOT NULL, title TEXT NOT NULL DEFAULT '', cover_url TEXT NOT NULL DEFAULT '',
	  work_count INTEGER NOT NULL DEFAULT 0, first_seen_at TEXT NOT NULL,
	  last_seen_at TEXT NOT NULL, UNIQUE(creator_id, mix_id));
	CREATE TABLE works (
	  id INTEGER PRIMARY KEY AUTOINCREMENT, creator_id INTEGER NOT NULL REFERENCES creators(id),
	  collection_id INTEGER REFERENCES collections(id), item_id TEXT NOT NULL UNIQUE,
	  title TEXT NOT NULL DEFAULT '', published_at TEXT, cover_url TEXT NOT NULL DEFAULT '',
	  duration INTEGER NOT NULL DEFAULT 0, media_url TEXT NOT NULL DEFAULT '',
	  raw_metadata TEXT NOT NULL DEFAULT '{}', first_seen_at TEXT NOT NULL);
	CREATE TABLE work_assets (
	  id INTEGER PRIMARY KEY AUTOINCREMENT, work_id INTEGER NOT NULL REFERENCES works(id),
	  job_id INTEGER, asset_index INTEGER NOT NULL DEFAULT 0,
	  asset_kind TEXT NOT NULL, bucket TEXT NOT NULL DEFAULT '', object_key TEXT NOT NULL,
	  content_type TEXT NOT NULL DEFAULT 'application/octet-stream',
	  file_size INTEGER NOT NULL DEFAULT 0, etag TEXT NOT NULL DEFAULT '',
	  created_at TEXT NOT NULL, UNIQUE(work_id, object_key));
	CREATE TABLE download_jobs (
	  id INTEGER PRIMARY KEY AUTOINCREMENT, work_id INTEGER NOT NULL REFERENCES works(id),
	  status TEXT NOT NULL DEFAULT 'queued', progress_bytes INTEGER NOT NULL DEFAULT 0,
	  total_bytes INTEGER NOT NULL DEFAULT 0, attempts INTEGER NOT NULL DEFAULT 0,
	  error_code TEXT, error_message TEXT, file_path TEXT, created_at TEXT NOT NULL,
	  started_at TEXT, finished_at TEXT, requested_quality TEXT NOT NULL DEFAULT 'highest',
	  actual_quality TEXT, UNIQUE(work_id, status));`
	if _, err := h.Exec(schema); err != nil {
		t.Fatal(err)
	}
	seed := []string{
		// creators: created_at, updated_at after the other columns in this
		// minimal layout is fine; keep the real column order though.
		`INSERT INTO creators (id, sec_user_id, profile_url, nickname, avatar_url, created_at, updated_at, reported_work_count)
		 VALUES (1, 'sec-wen', 'http://p/wen', 'wen_', 'http://a/wen', '2026-07-19T06:52:46+00:00', '2026-07-19T06:52:46+00:00', 42)`,
		`INSERT INTO collections (id, creator_id, mix_id, title, cover_url, first_seen_at, last_seen_at)
		 VALUES (10, 1, 'mix-1', '合集甲', 'http://c/1', '2026-07-19T07:00:00+00:00', '2026-07-19T07:00:00+00:00')`,
		`INSERT INTO works (id, creator_id, collection_id, item_id, title, published_at, cover_url, duration, first_seen_at)
		 VALUES (100, 1, 10, 'item-100', '视频一', '2026-07-18T07:42:32+00:00', 'http://cover/100', 65000, '2026-07-19T07:01:00+00:00')`,
		`INSERT INTO works (id, creator_id, collection_id, item_id, title, published_at, cover_url, duration, first_seen_at)
		 VALUES (101, 1, NULL, 'item-101', '视频二', '2026-07-16T09:42:28+00:00', 'http://cover/101', 0, '2026-07-19T07:02:00+00:00')`,
		`INSERT INTO download_jobs (id, work_id, status, progress_bytes, total_bytes, attempts, created_at, started_at, finished_at, requested_quality, actual_quality)
		 VALUES (50, 100, 'succeeded', 1000, 1000, 1, '2026-07-19T07:03:00+00:00', '2026-07-19T07:03:01+00:00', '2026-07-19T07:03:30+00:00', 'highest', '720')`,
		`INSERT INTO download_jobs (id, work_id, status, progress_bytes, total_bytes, attempts, created_at, started_at, finished_at, requested_quality, actual_quality)
		 VALUES (51, 101, 'succeeded', 0, 0, 2, '2026-07-19T07:04:00+00:00', '2026-07-19T07:04:01+00:00', '2026-07-19T07:04:30+00:00', 'highest', NULL)`,
		`INSERT INTO download_jobs (id, work_id, status, attempts, created_at, requested_quality, actual_quality)
		 VALUES (52, 100, 'failed', 1, '2026-07-19T07:02:00+00:00', 'highest', NULL)`,
		// Work 100: video (exists on disk), metadata, cover image.
		`INSERT INTO work_assets (id, work_id, job_id, asset_kind, object_key, file_size, created_at)
		 VALUES (60, 100, 50, 'video', 'wen_/singles/a.mp4', 1000, '2026-07-19T07:03:30+00:00')`,
		`INSERT INTO work_assets (id, work_id, job_id, asset_kind, object_key, file_size, created_at)
		 VALUES (61, 100, 50, 'metadata', 'wen_/singles/a.json', 200, '2026-07-19T07:03:30+00:00')`,
		`INSERT INTO work_assets (id, work_id, job_id, asset_kind, object_key, file_size, created_at)
		 VALUES (62, 100, NULL, 'image', 'wen_/singles/a.jpg', 300, '2026-07-19T07:03:31+00:00')`,
		// Work 101: video row exists but the file is NOT on disk.
		`INSERT INTO work_assets (id, work_id, job_id, asset_kind, object_key, file_size, created_at)
		 VALUES (63, 101, 51, 'video', 'wen_/singles/b.mp4', 999, '2026-07-19T07:04:30+00:00')`,
	}
	for _, s := range seed {
		if _, err := h.Exec(s); err != nil {
			t.Fatal(err)
		}
	}

	// Downloads tree: only work 100's files exist on disk.
	if err := os.MkdirAll(filepath.Join(downloads, "wen_", "singles"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, size := range map[string]int{"a.mp4": 1000, "a.json": 200, "a.jpg": 300} {
		if err := os.WriteFile(filepath.Join(downloads, "wen_", "singles", name), make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return legacyDB, downloads, dataDir
}

func runOpts(legacyDB, downloads, dataDir string, copy bool) Options {
	return Options{
		LegacyDB:   legacyDB,
		LegacyRoot: downloads,
		DataDir:    dataDir,
		DBPath:     filepath.Join(dataDir, "test.db"),
		CopyFiles:  copy,
		BatchSize:  2, // small to exercise batching
	}
}

func openTarget(t *testing.T, path string) *sql.DB {
	t.Helper()
	h, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	return h
}

func TestRunMigratesAndIsIdempotent(t *testing.T) {
	legacyDB, downloads, dataDir := fixture(t)
	opts := runOpts(legacyDB, downloads, dataDir, true) // copy mode: hermetic

	rep, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if rep.Creators.Migrated != 1 || rep.Collections.Migrated != 1 || rep.Works.Migrated != 2 {
		t.Fatalf("first run counts: %+v", rep)
	}
	// Assets: video+metadata+cover of work 100; work 101's video is missing.
	if rep.Assets.Migrated != 3 || rep.MissingFiles != 1 {
		t.Fatalf("assets: migrated=%d missing=%d (%+v)", rep.Assets.Migrated, rep.MissingFiles, rep)
	}
	// Only work 100's succeeded job has a validated video.
	if rep.Jobs.Migrated != 1 || rep.SkippedNoVid != 1 || rep.SkippedOther != 1 {
		t.Fatalf("jobs: migrated=%d noVid=%d other=%d (%+v)", rep.Jobs.Migrated, rep.SkippedNoVid, rep.SkippedOther, rep)
	}
	if rep.FilesCopied != 3 {
		t.Fatalf("files copied = %d, want 3", rep.FilesCopied)
	}

	// Row-level checks.
	h := openTarget(t, opts.DBPath)
	var published, publishedType string
	if err := h.QueryRow(`SELECT published_at, typeof(published_at) FROM works WHERE item_id = 'item-100'`).
		Scan(&published, &publishedType); err != nil {
		t.Fatal(err)
	}
	if published != "2026-07-18T07:42:32+00:00" || publishedType != "text" {
		t.Fatalf("published_at not preserved verbatim: %q (%s)", published, publishedType)
	}
	var kind, path string
	var quality sql.NullString
	if err := h.QueryRow(`SELECT kind, path, quality FROM assets WHERE kind='video'`).
		Scan(&kind, &path, &quality); err != nil {
		t.Fatal(err)
	}
	if path != "downloads/legacy/wen_/singles/a.mp4" || !quality.Valid || quality.String != "720p" {
		t.Fatalf("video asset: kind=%s path=%s quality=%v", kind, path, quality)
	}
	var n int
	if err := h.QueryRow(`SELECT COUNT(*) FROM assets WHERE kind='cover'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("cover assets = %d (%v)", n, err)
	}
	var jobQ, jobStatus string
	var jobBytes int64
	if err := h.QueryRow(`SELECT quality, status, total_bytes FROM download_jobs`).
		Scan(&jobQ, &jobStatus, &jobBytes); err != nil {
		t.Fatal(err)
	}
	if jobQ != "720p" || jobStatus != "succeeded" || jobBytes != 1000 {
		t.Fatalf("job: quality=%s status=%s bytes=%d", jobQ, jobStatus, jobBytes)
	}
	// raw_metadata must never leak into the new schema.
	if _, err := h.Query(`SELECT raw_metadata FROM works`); err == nil {
		t.Fatal("new works table should not have raw_metadata")
	}

	// Second run: everything skipped, counts unchanged. The missing video is
	// re-reported (still absent), its already-migrated siblings are skips.
	rep2, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if rep2.Creators.Migrated+rep2.Collections.Migrated+rep2.Works.Migrated+rep2.Assets.Migrated+rep2.Jobs.Migrated != 0 {
		t.Fatalf("second run inserted rows: %+v", rep2)
	}
	if rep2.Creators.Skipped != 1 || rep2.Collections.Skipped != 1 || rep2.Works.Skipped != 2 ||
		rep2.Assets.Skipped != 4 || rep2.MissingFiles != 1 ||
		rep2.Jobs.Skipped != 1 || rep2.SkippedNoVid != 1 || rep2.SkippedOther != 1 {
		t.Fatalf("second run skips: %+v", rep2)
	}
	if rep2.Totals["works"] != 2 || rep2.Totals["assets"] != 3 || rep2.Totals["download_jobs"] != 1 {
		t.Fatalf("second run totals: %+v", rep2.Totals)
	}
}

func TestRunJunctionMakesLegacyFilesResolvable(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("junction test is Windows-only")
	}
	legacyDB, downloads, dataDir := fixture(t)
	opts := runOpts(legacyDB, downloads, dataDir, false)

	rep, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	link := filepath.Join(dataDir, "downloads", junctionName)
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("junction not created: %v (%s)", err, rep.Junction)
	}
	// The migrated asset must resolve through the junction.
	if _, err := os.Stat(filepath.Join(link, "wen_", "singles", "a.mp4")); err != nil {
		t.Fatalf("legacy file not reachable through junction: %v", err)
	}
	// Rerun must report the junction as already present, not recreate it.
	rep2, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if rep2.Junction == "" || !strings.Contains(rep2.Junction, "already present") {
		t.Fatalf("rerun junction status: %q", rep2.Junction)
	}
}

func TestRunRefusesLegacyAsTarget(t *testing.T) {
	legacyDB, downloads, _ := fixture(t)
	opts := runOpts(legacyDB, downloads, t.TempDir(), true)
	opts.DBPath = legacyDB // misconfiguration: would overwrite the legacy db
	if _, err := Run(context.Background(), opts); err == nil {
		t.Fatal("expected refusal when target db == legacy db")
	}
}
