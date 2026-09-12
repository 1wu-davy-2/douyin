// Package legacy migrates a v1 (Python era) douyin-archive SQLite database
// into the v2 schema so the already-downloaded corpus becomes playable in the
// new UI without re-downloading.
//
// Migration contract (stage 7):
//
//   - creators: sec_uid/nickname/avatar/profile_url/reported_work_count/
//     created_at are preserved. last_scan_at is deliberately NOT migrated
//     (the new scanner records runs in scan_runs; the column means something
//     different here).
//   - collections: mix_id/name/cover_url, idempotent per (creator, mix_id).
//     Old column `title` becomes new column `name`; created_at uses the old
//     first_seen_at.
//   - works: item_id/title/cover_url/duration/published_at are preserved
//     (published_at keeps its original ISO-8601 string form). The old
//     media_url column is dropped (the new tool resolves media URLs at
//     download time) and the huge raw_metadata column (~59 KB per row) is
//     never read - every legacy query lists explicit columns. created_at/
//     updated_at use the old first_seen_at; deleted_at stays NULL. The
//     collection link is remapped old collections.id -> new collections.id.
//   - work_assets -> assets. Kind mapping (stage 9): video->video (tier
//     quality from the linked job), image->image with a 4-digit sequence
//     quality ("0001", "0002", ... numbered per work by the old
//     created_at/object_key order), live_photo->video with a "live0001"
//     sequence quality (the old live_photo rows are the motion clips of live
//     slides, always video/mp4 - the same shape the v2 downloader produces
//     for gallery works), metadata->metadata. Nothing collapses into the
//     cover slot anymore. Video tier assets are only migrated when the file
//     actually exists under the legacy downloads root; missing files are
//     counted and reported as warnings. When several old rows share the v2
//     UNIQUE slot (work_id, kind, quality), the newest (created_at DESC)
//     wins it.
//   - works.type is inferred after the asset pass: any work holding image
//     assets becomes type='image' (gallery), the rest stay 'video'.
//   - download_jobs: only succeeded jobs whose video asset survived disk
//     validation are rebuilt (status=succeeded, quality=asset quality,
//     total_bytes=asset size, finished_at from the old row). All other
//     history is dropped - the user asked for a clean finished library.
//   - subscriptions / provider_state / runtime_settings / scan_runs: not
//     migrated (the new tool reconfigures these; their semantics changed).
//
// Paths: assets.path is stored relative to the data dir with forward slashes
// ("downloads/legacy/<object_key>"), so it resolves through the junction; the
// v2 downloader stores its own rows under "downloads/{nickname}_{sec_uid}/"
// in the same directory. By default the files are NOT copied: a directory
// junction <data>/downloads/legacy -> <legacy downloads root> (cmd /c mklink
// /J on Windows) makes the corpus playable with zero disk cost. -copy-files
// performs a real copy into the same layout instead. An existing junction is
// never recreated.
//
// The legacy database is opened strictly read-only (file:...?mode=ro with
// foreign_keys off) and is never written by this package.
package legacy

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"douyin/backend/internal/db"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, same as the server
)

// Defaults chosen for this repository's layout (run from backend/, ../data is
// the repo-root data directory).
const (
	DefaultLegacyDB   = `E:\home\douyin-archive\data\app.db`
	DefaultLegacyRoot = `E:\home\douyin-archive\data\downloads`
	DefaultDataDir    = "../data"

	// junctionName is the directory created under <data-dir>/downloads that
	// links to the legacy downloads root. A dedicated subdirectory (instead
	// of linking data/downloads itself) coexists with content the v2
	// downloader has already written there.
	junctionName = "legacy"

	// legacyPrefix is the assets.path prefix for every migrated file; it
	// resolves through <data-dir>/downloads/legacy in both linkage modes.
	legacyPrefix = "downloads/legacy"

	defaultBatch = 500
)

// Options is the resolved configuration for one migration run.
type Options struct {
	LegacyDB   string // old app.db (read-only)
	LegacyRoot string // old downloads root (junction/copy source)
	DataDir    string // new tool data directory
	DBPath     string // new SQLite file; "" -> <DataDir>/app.db
	CopyFiles  bool   // copy files instead of junctioning
	BatchSize  int    // rows per transaction
}

// Main is the entry point shared by cmd/import-legacy and the server's
// -mode import-legacy dispatch. It parses args, runs the migration and
// returns the process exit code (0 on success, even with missing files; 1 on
// fatal errors).
func Main(args []string) int {
	fs := flag.NewFlagSet("import-legacy", flag.ExitOnError)
	opts := Options{}
	fs.StringVar(&opts.LegacyDB, "legacy-db", DefaultLegacyDB, "legacy v1 SQLite database (opened READ-ONLY)")
	fs.StringVar(&opts.LegacyRoot, "legacy-downloads-root", DefaultLegacyRoot, "legacy downloads root directory")
	fs.StringVar(&opts.DataDir, "data-dir", DefaultDataDir, "new tool data directory (default ../data relative to the repo when run from backend/)")
	fs.StringVar(&opts.DBPath, "db-path", "", "new SQLite file (default <data-dir>/app.db)")
	fs.BoolVar(&opts.CopyFiles, "copy-files", false, "copy legacy files into the data dir instead of creating a junction")
	fs.IntVar(&opts.BatchSize, "batch", defaultBatch, "rows per transaction")
	// Accepted for compatibility: `douyin-server.exe -mode import-legacy ...`
	// hands the full argument list over, including -mode itself.
	_ = fs.String("mode", "", "ignored (compatibility with -mode import-legacy)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "import-legacy - migrate the v1 douyin-archive database into the v2 schema")
		fmt.Fprintln(fs.Output(), `
The legacy database is only ever opened read-only. By default the already
downloaded files are linked (not copied) via a directory junction
<data>/downloads/legacy -> <legacy-downloads-root>.`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = defaultBatch
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	start := time.Now()
	rep, err := Run(ctx, opts)
	if err != nil {
		log.Printf("[import-legacy] FAILED: %v", err)
		return 1
	}
	printReport(opts, rep, time.Since(start))
	return 0
}

// tableStat is the reconciliation count for one table.
type tableStat struct {
	Migrated int
	Skipped  int
}

// Report is the end-of-run reconciliation summary.
type Report struct {
	Creators     tableStat
	Collections  tableStat
	Works        tableStat
	Assets       tableStat
	Jobs         tableStat
	SkippedNoVid int // old succeeded jobs without a validated video asset
	SkippedOther int // old jobs not in 'succeeded' state

	MissingFiles int // video assets whose file is absent on disk
	FilesCopied  int // files copied (copy mode only)
	Junction     string
	Warnings     []string
	Totals       map[string]int // final row counts in the new database
}

// Run executes the migration and returns the reconciliation report. It is
// idempotent: rerunning against an already-migrated target inserts nothing.
func Run(ctx context.Context, opts Options) (Report, error) {
	var rep Report

	if opts.LegacyDB == "" {
		return rep, errors.New("legacy db path is empty")
	}
	if opts.DataDir == "" {
		return rep, errors.New("data dir is empty")
	}
	if opts.DBPath == "" {
		opts.DBPath = filepath.Join(opts.DataDir, "app.db")
	}

	// Safety net: never let the target be the legacy file itself.
	if samePath(opts.LegacyDB, opts.DBPath) {
		return rep, fmt.Errorf("refusing: target db %s is the legacy database itself", opts.DBPath)
	}
	if st, err := os.Stat(opts.LegacyDB); err != nil || st.IsDir() {
		return rep, fmt.Errorf("legacy db not found: %s", opts.LegacyDB)
	}

	if err := os.MkdirAll(opts.DataDir, 0o755); err != nil {
		return rep, fmt.Errorf("create data dir %s: %w", opts.DataDir, err)
	}

	// New database: full read-write setup + schema migrations.
	target, err := db.Open(opts.DBPath)
	if err != nil {
		return rep, err
	}
	defer target.Close()
	if err := db.Migrate(target); err != nil {
		return rep, fmt.Errorf("migrate target schema: %w", err)
	}

	// Legacy database: strictly read-only connection.
	legacy, err := openLegacyRO(opts.LegacyDB)
	if err != nil {
		return rep, err
	}
	defer legacy.Close()

	m := &migrator{
		ctx:        ctx,
		legacy:     legacy,
		target:     target,
		opts:       opts,
		rep:        &rep,
		creatorMap: map[int64]int64{}, // filled by migrateCreators
		collMap:    map[int64]int64{}, // filled by migrateCollections
		valid:      map[int64]videoRef{},
	}
	if err := m.run(); err != nil {
		return rep, err
	}
	return rep, nil
}

// ------------------------------------------------------------ legacy open --

// openLegacyRO opens the legacy database with a read-only file: URI. Foreign
// keys stay off (the old schema's cascades are irrelevant for reads) and a
// single connection serializes our reads.
func openLegacyRO(path string) (*sql.DB, error) {
	handle, err := sql.Open("sqlite", roDSN(path))
	if err != nil {
		return nil, fmt.Errorf("open legacy db %s: %w", path, err)
	}
	handle.SetMaxOpenConns(1)
	if err := handle.Ping(); err != nil {
		handle.Close()
		return nil, fmt.Errorf("open legacy db %s (read-only): %w", path, err)
	}
	return handle, nil
}

// roDSN builds file:...?mode=ro&_pragma=foreign_keys(0) with the same path
// escaping rules as internal/db (absolute, forward slashes, empty authority).
// No journal pragma is set: a read-only connection must not attempt to change
// the journal mode of the legacy file.
func roDSN(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	p := filepath.ToSlash(path)
	if runtime.GOOS == "windows" && !strings.HasPrefix(p, "/") {
		p = "/" + p // file:///E:/... — empty URI authority
	}
	u := url.URL{Scheme: "file", Path: p}
	return u.String() + "?mode=ro&_pragma=foreign_keys(0)&_pragma=busy_timeout(5000)"
}

// samePath reports whether two paths denote the same file (case-insensitive,
// absolute comparison; enough as a misconfiguration guard).
func samePath(a, b string) bool {
	abs := func(p string) string {
		if r, err := filepath.Abs(p); err == nil {
			return strings.ToLower(filepath.Clean(r))
		}
		return strings.ToLower(filepath.Clean(p))
	}
	return abs(a) == abs(b)
}

// ---------------------------------------------------------------- migrator --

type workRef struct {
	id      int64 // new works.id
	creator int64 // new creators.id
}

type videoRef struct {
	quality string
	size    int64
}

type fileCopy struct {
	src, dst string
}

type migrator struct {
	ctx    context.Context
	legacy *sql.DB
	target *sql.DB
	opts   Options
	rep    *Report

	creatorBySec map[string]int64 // new db: sec_uid -> id
	creatorMap   map[int64]int64  // old creator id -> new id
	collByMix    map[string]int64 // new db: "<creator>\x00<mix_id>" -> id
	collMap      map[int64]int64  // old collection id -> new id
	workByItem   map[string]workRef
	workByOld    map[int64]workRef
	valid        map[int64]videoRef       // old work id -> validated video asset
	imgSeq       map[int64]map[string]int // old work id -> object_key -> image sequence (1-based)
	liveSeq      map[int64]map[string]int // old work id -> object_key -> live clip sequence (1-based)
	copies       []fileCopy               // pending file copies (copy mode)
}

func (m *migrator) run() error {
	if err := m.preload(); err != nil {
		return err
	}
	if err := m.migrateCreators(); err != nil {
		return fmt.Errorf("creators: %w", err)
	}
	if err := m.migrateCollections(); err != nil {
		return fmt.Errorf("collections: %w", err)
	}
	if err := m.migrateWorks(); err != nil {
		return fmt.Errorf("works: %w", err)
	}
	if err := m.migrateAssets(); err != nil {
		return fmt.Errorf("assets: %w", err)
	}
	if err := m.applyWorkTypes(); err != nil {
		return fmt.Errorf("work type inference: %w", err)
	}
	if err := m.migrateJobs(); err != nil {
		return fmt.Errorf("download_jobs: %w", err)
	}
	if err := m.linkFiles(); err != nil {
		m.warn("file linkage: %v", err)
	}
	return m.collectTotals()
}

// warn records a non-fatal problem; the migration continues and the report
// lists every warning.
func (m *migrator) warn(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	m.rep.Warnings = append(m.rep.Warnings, msg)
	log.Printf("[import-legacy] warning: %s", msg)
}

// preload loads the target database's existing identity maps so reruns skip
// everything without rewriting a single row.
func (m *migrator) preload() error {
	m.creatorBySec = map[string]int64{}
	if err := m.scan(`SELECT id, sec_uid FROM creators`, func(rows *sql.Rows) error {
		var id int64
		var sec string
		if err := rows.Scan(&id, &sec); err != nil {
			return err
		}
		m.creatorBySec[sec] = id
		return nil
	}); err != nil {
		return fmt.Errorf("preload creators: %w", err)
	}

	m.collByMix = map[string]int64{}
	if err := m.scan(`SELECT id, creator_id, mix_id FROM collections`, func(rows *sql.Rows) error {
		var id, creator int64
		var mix string
		if err := rows.Scan(&id, &creator, &mix); err != nil {
			return err
		}
		m.collByMix[collKey(creator, mix)] = id
		return nil
	}); err != nil {
		return fmt.Errorf("preload collections: %w", err)
	}

	m.workByItem = map[string]workRef{}
	m.workByOld = map[int64]workRef{}
	if err := m.scan(`SELECT id, creator_id, item_id FROM works`, func(rows *sql.Rows) error {
		var id, creator int64
		var item string
		if err := rows.Scan(&id, &creator, &item); err != nil {
			return err
		}
		m.workByItem[itemKey(creator, item)] = workRef{id: id, creator: creator}
		return nil
	}); err != nil {
		return fmt.Errorf("preload works: %w", err)
	}

	// Video assets already in the target (previous run or new-tool download)
	// count as validated for the download_jobs rebuild; keyed by new work id.
	// Live-photo clips (quality "live%04d") never qualify: they are gallery
	// segments, not the downloadable video tier a succeeded job refers to.
	m.valid = map[int64]videoRef{}
	if err := m.scan(`SELECT work_id, quality, size_bytes FROM assets
		WHERE kind = 'video' AND (quality IS NULL OR quality NOT LIKE 'live%')`, func(rows *sql.Rows) error {
		var workID, size int64
		var quality sql.NullString
		if err := rows.Scan(&workID, &quality, &size); err != nil {
			return err
		}
		m.valid[workID] = videoRef{quality: quality.String, size: size}
		return nil
	}); err != nil {
		return fmt.Errorf("preload assets: %w", err)
	}

	// Gallery sequence numbers: per work, old image rows get "0001"... and
	// live_photo rows get "live0001"... in the old created_at/object_key
	// order (the old archiver wrote them in slide order). object_key is
	// UNIQUE per work, so it keys the inner maps.
	m.imgSeq = map[int64]map[string]int{}
	m.liveSeq = map[int64]map[string]int{}
	if err := m.scanLegacy(`
		SELECT work_id, asset_kind, object_key FROM work_assets
		WHERE asset_kind IN ('image', 'live_photo')
		ORDER BY work_id, created_at, object_key, id`, func(rows *sql.Rows) error {
		var workID int64
		var kind, objectKey string
		if err := rows.Scan(&workID, &kind, &objectKey); err != nil {
			return err
		}
		seq := m.imgSeq
		if kind == "live_photo" {
			seq = m.liveSeq
		}
		if seq[workID] == nil {
			seq[workID] = map[string]int{}
		}
		seq[workID][objectKey] = len(seq[workID]) + 1
		return nil
	}); err != nil {
		return fmt.Errorf("preload gallery sequences: %w", err)
	}
	return nil
}

// scan is a small rows-iteration helper that always closes the cursor.
func (m *migrator) scan(query string, fn func(*sql.Rows) error) error {
	rows, err := m.target.QueryContext(m.ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// scanLegacy is scan over the read-only legacy database.
func (m *migrator) scanLegacy(query string, fn func(*sql.Rows) error) error {
	rows, err := m.legacy.QueryContext(m.ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

func collKey(creator int64, mix string) string { return strconv.FormatInt(creator, 10) + "\x00" + mix }
func itemKey(creator int64, item string) string {
	return strconv.FormatInt(creator, 10) + "\x00" + item
}

// -------------------------------------------------------------- creators ---

func (m *migrator) migrateCreators() error {
	rows, err := m.legacy.QueryContext(m.ctx, `
		SELECT id, sec_user_id, nickname, avatar_url, profile_url, reported_work_count, created_at
		FROM creators ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()

	bw := newBatchWriter(m.ctx, m.target, m.opts.BatchSize, `
		INSERT INTO creators (sec_uid, nickname, avatar_url, profile_url, reported_work_count, created_at, last_scan_at)
		VALUES (?, ?, ?, ?, ?, ?, NULL)`, "creators")
	defer bw.rollbackIfOpen()

	for rows.Next() {
		if err := m.ctx.Err(); err != nil {
			return err
		}
		var oldID, reported int64
		var sec, created string
		var nickname, avatar, profile string
		if err := rows.Scan(&oldID, &sec, &nickname, &avatar, &profile, &reported, &created); err != nil {
			return err
		}
		if newID, ok := m.creatorBySec[sec]; ok {
			m.creatorMap[oldID] = newID
			m.rep.Creators.Skipped++
			continue
		}
		res, err := bw.exec(sec, nickname, avatar, profile, reported, created)
		if err != nil {
			return err
		}
		newID, err := res.LastInsertId()
		if err != nil {
			return err
		}
		m.creatorBySec[sec] = newID
		m.creatorMap[oldID] = newID
		m.rep.Creators.Migrated++
	}
	if err := rows.Err(); err != nil {
		return err // deferred rollback aborts the unfinished batch
	}
	return bw.finish()
}

// ----------------------------------------------------------- collections ---

func (m *migrator) migrateCollections() error {
	rows, err := m.legacy.QueryContext(m.ctx, `
		SELECT id, creator_id, mix_id, title, cover_url, first_seen_at
		FROM collections ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()

	bw := newBatchWriter(m.ctx, m.target, m.opts.BatchSize, `
		INSERT INTO collections (creator_id, mix_id, name, cover_url, created_at)
		VALUES (?, ?, ?, ?, ?)`, "collections")
	defer bw.rollbackIfOpen()

	for rows.Next() {
		if err := m.ctx.Err(); err != nil {
			return err
		}
		var oldID, oldCreator int64
		var mix, created string
		var title, cover string
		if err := rows.Scan(&oldID, &oldCreator, &mix, &title, &cover, &created); err != nil {
			return err
		}
		newCreator, ok := m.creatorMap[oldCreator]
		if !ok {
			m.warn("collection %d: creator %d not migrated, skipping", oldID, oldCreator)
			m.rep.Collections.Skipped++
			continue
		}
		if newID, ok := m.collByMix[collKey(newCreator, mix)]; ok {
			m.collMap[oldID] = newID
			m.rep.Collections.Skipped++
			continue
		}
		res, err := bw.exec(newCreator, mix, title, cover, created)
		if err != nil {
			return err
		}
		newID, err := res.LastInsertId()
		if err != nil {
			return err
		}
		m.collByMix[collKey(newCreator, mix)] = newID
		m.collMap[oldID] = newID
		m.rep.Collections.Migrated++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return bw.finish()
}

// ----------------------------------------------------------------- works ---

func (m *migrator) migrateWorks() error {
	rows, err := m.legacy.QueryContext(m.ctx, `
		SELECT id, creator_id, collection_id, item_id, title, cover_url, duration, published_at, first_seen_at
		FROM works ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()

	bw := newBatchWriter(m.ctx, m.target, m.opts.BatchSize, `
		INSERT INTO works (creator_id, collection_id, item_id, title, cover_url, duration, published_at, deleted_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, NULL, ?, ?)`, "works")
	defer bw.rollbackIfOpen()

	for rows.Next() {
		if err := m.ctx.Err(); err != nil {
			return err
		}
		var oldID, oldCreator, duration int64
		var item, created string
		var title, cover string
		var oldColl sql.NullInt64
		var published sql.NullString
		if err := rows.Scan(&oldID, &oldCreator, &oldColl, &item, &title, &cover, &duration, &published, &created); err != nil {
			return err
		}
		newCreator, ok := m.creatorMap[oldCreator]
		if !ok {
			m.warn("work %d (item %s): creator %d not migrated, skipping", oldID, item, oldCreator)
			m.rep.Works.Skipped++
			continue
		}
		key := itemKey(newCreator, item)
		if ref, ok := m.workByItem[key]; ok {
			m.workByOld[oldID] = ref
			m.rep.Works.Skipped++
			continue
		}

		// Old collection_id -> new collections.id; dangling links (should not
		// happen - the old schema enforces the FK) degrade to NULL.
		var newColl sql.NullInt64
		if oldColl.Valid {
			if id, ok := m.collMap[oldColl.Int64]; ok {
				newColl = sql.NullInt64{Int64: id, Valid: true}
			} else {
				m.warn("work %d (item %s): collection %d not migrated, link dropped", oldID, item, oldColl.Int64)
			}
		}

		// published_at keeps the original ISO-8601 string byte for byte;
		// created_at/updated_at fall back to the old first_seen_at.
		res, err := bw.exec(newCreator, newColl, item, title, cover, duration, published, created, created)
		if err != nil {
			return err
		}
		newID, err := res.LastInsertId()
		if err != nil {
			return err
		}
		ref := workRef{id: newID, creator: newCreator}
		m.workByItem[key] = ref
		m.workByOld[oldID] = ref
		m.rep.Works.Migrated++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return bw.finish()
}

// ---------------------------------------------------------------- assets ---

// legacyKind maps old asset_kind values onto the v2 contract. image stills
// become kind=image with a 4-digit sequence; live_photo rows (the old
// archiver stored them as video/mp4 motion clips) become kind=video with a
// "live%04d" quality - the same shape the v2 downloader produces for gallery
// works - so the slideshow player renders them correctly.
func legacyKind(kind string) (string, bool) {
	switch kind {
	case "video":
		return "video", true
	case "image":
		return "image", true
	case "live_photo":
		return "live", true // v2 kind=video with "live"+sequence quality
	case "metadata":
		return "metadata", true
	}
	return "", false
}

// qualityTier normalizes a bare numeric tier to the new ladder ("720" ->
// "720p"); "original"/"highest" and already-suffixed tiers pass through.
func qualityTier(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return ""
	}
	if n, err := strconv.Atoi(q); err == nil {
		return strconv.Itoa(n) + "p"
	}
	return q
}

// normalizeQuality resolves an asset quality: the old job's actual tier
// first, then the requested tier, else "" (NULL / unknown).
func normalizeQuality(actual, requested sql.NullString) string {
	if actual.Valid {
		if v := qualityTier(actual.String); v != "" {
			return v
		}
	}
	if requested.Valid {
		return qualityTier(requested.String)
	}
	return ""
}

func (m *migrator) migrateAssets() error {
	// Newest-first so that when several old rows share the v2 UNIQUE slot
	// (work_id, kind, quality) the newest one wins it and later duplicates
	// are reported as skipped. raw_metadata/media_url are never referenced.
	rows, err := m.legacy.QueryContext(m.ctx, `
		SELECT a.work_id, a.asset_kind, a.object_key, a.file_size, a.created_at,
		       j.actual_quality, j.requested_quality
		FROM work_assets a
		LEFT JOIN download_jobs j ON j.id = a.job_id
		ORDER BY a.created_at DESC, a.id DESC`)
	if err != nil {
		return err
	}
	defer rows.Close()

	bw := newBatchWriter(m.ctx, m.target, m.opts.BatchSize, `
		INSERT INTO assets (work_id, kind, path, size_bytes, quality, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, "assets")
	defer bw.rollbackIfOpen()

	legacyRoot, err := filepath.Abs(m.opts.LegacyRoot)
	if err != nil {
		legacyRoot = m.opts.LegacyRoot
	}

	for rows.Next() {
		if err := m.ctx.Err(); err != nil {
			return err
		}
		var oldWork int64
		var kind, objectKey, created string
		var size int64
		var actual, requested sql.NullString
		if err := rows.Scan(&oldWork, &kind, &objectKey, &size, &created, &actual, &requested); err != nil {
			return err
		}
		newKind, ok := legacyKind(kind)
		if !ok {
			m.warn("asset for work %d: unknown kind %q, skipping", oldWork, kind)
			m.rep.Assets.Skipped++
			continue
		}
		ref, ok := m.workByOld[oldWork]
		if !ok {
			m.warn("asset %s: work %d not migrated, skipping", objectKey, oldWork)
			m.rep.Assets.Skipped++
			continue
		}

		// Stored path: relative to the data dir, forward slashes, through the
		// legacy link directory (same convention in junction and copy mode).
		stored := legacyPrefix + "/" + filepath.ToSlash(objectKey)
		full := filepath.Join(legacyRoot, filepath.FromSlash(objectKey))

		// Gallery rows carry their preloaded sequence number as quality;
		// live clips land in the video kind with a "live%04d" quality.
		liveClip := newKind == "live"
		quality := sql.NullString{}
		if newKind == "image" {
			seq, ok := m.imgSeq[oldWork][objectKey]
			if !ok {
				m.warn("asset %s: no gallery sequence for work %d, skipping", objectKey, oldWork)
				m.rep.Assets.Skipped++
				continue
			}
			quality = sql.NullString{String: fmt.Sprintf("%04d", seq), Valid: true}
			newKind = "image"
		} else if liveClip {
			seq, ok := m.liveSeq[oldWork][objectKey]
			if !ok {
				m.warn("asset %s: no live sequence for work %d, skipping", objectKey, oldWork)
				m.rep.Assets.Skipped++
				continue
			}
			quality = sql.NullString{String: fmt.Sprintf("live%04d", seq), Valid: true}
			newKind = "video"
		} else if newKind == "video" {
			if q := normalizeQuality(actual, requested); q != "" {
				quality = sql.NullString{String: q, Valid: true}
			}
			// Disk validation: migrate video assets only when the file exists.
			if _, err := os.Stat(full); err != nil {
				m.rep.MissingFiles++
				m.rep.Assets.Skipped++
				log.Printf("[import-legacy] missing video file (skipped): %s", full)
				continue
			}
		}

		// Idempotency: video rows are unique per (work, kind, quality);
		// NULL-quality rows (cover/metadata) per (work, kind, path).
		check := `SELECT 1 FROM assets WHERE work_id = ? AND kind = ? AND quality = ?`
		qargs := []any{ref.id, newKind, quality.String}
		if !quality.Valid {
			check = `SELECT 1 FROM assets WHERE work_id = ? AND kind = ? AND quality IS NULL AND path = ?`
			qargs = []any{ref.id, newKind, stored}
		}
		var exists int
		err := m.target.QueryRowContext(m.ctx, check, qargs...).Scan(&exists)
		switch {
		case err == nil:
			m.rep.Assets.Skipped++
			continue
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}

		if _, err := bw.exec(ref.id, newKind, stored, size, quality, created); err != nil {
			return err
		}
		m.rep.Assets.Migrated++

		if newKind == "video" && !liveClip {
			// Register the validated video for the download_jobs rebuild; the
			// first (newest, thanks to the DESC ordering) row wins. Live
			// clips never qualify (see preload).
			if _, ok := m.valid[ref.id]; !ok {
				m.valid[ref.id] = videoRef{quality: quality.String, size: size}
			}
		}
		if m.opts.CopyFiles {
			m.copies = append(m.copies, fileCopy{src: full, dst: filepath.Join(m.opts.DataDir, filepath.FromSlash(stored))})
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return bw.finish()
}

// ------------------------------------------------------------------ jobs ---

// applyWorkTypes infers works.type from the migrated assets (stage 9): any
// work holding image assets is a gallery (type='image'); the rest keep the
// 'video' default. Idempotent and safe for works already present in the
// target: the v2 downloader records image assets only on image works.
func (m *migrator) applyWorkTypes() error {
	res, err := m.target.ExecContext(m.ctx, `
		UPDATE works SET type = 'image'
		WHERE type <> 'image' AND id IN (
			SELECT DISTINCT work_id FROM assets WHERE kind = 'image')`)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("[import-legacy] works: %d row(s) marked type='image' (gallery assets)", n)
	}
	return nil
}

func (m *migrator) migrateJobs() error {
	rows, err := m.legacy.QueryContext(m.ctx, `
		SELECT id, work_id, status, attempts, created_at, started_at, finished_at
		FROM download_jobs ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()

	bw := newBatchWriter(m.ctx, m.target, m.opts.BatchSize, `
		INSERT INTO download_jobs (work_id, creator_id, quality, status, attempts, total_bytes, downloaded_bytes, error, queued_at, started_at, finished_at)
		VALUES (?, ?, ?, 'succeeded', ?, ?, ?, NULL, ?, ?, ?)`, "download_jobs")
	defer bw.rollbackIfOpen()

	for rows.Next() {
		if err := m.ctx.Err(); err != nil {
			return err
		}
		var oldID, oldWork, attempts int64
		var status, created string
		var started, finished sql.NullString
		if err := rows.Scan(&oldID, &oldWork, &status, &attempts, &created, &started, &finished); err != nil {
			return err
		}
		if status != "succeeded" {
			m.rep.SkippedOther++
			continue
		}
		ref, ok := m.workByOld[oldWork]
		if !ok {
			m.warn("job %d: work %d not migrated, skipping", oldID, oldWork)
			m.rep.SkippedNoVid++
			continue
		}
		vid, ok := m.valid[ref.id]
		if !ok {
			// Succeeded but no video asset survived disk validation: the user
			// wants a clean finished library, so the job is not rebuilt.
			m.rep.SkippedNoVid++
			continue
		}

		// Never shadow a job the new tool itself recorded as succeeded.
		var exists int
		err := m.target.QueryRowContext(m.ctx,
			`SELECT 1 FROM download_jobs WHERE work_id = ? AND status = 'succeeded' LIMIT 1`,
			ref.id).Scan(&exists)
		switch {
		case err == nil:
			m.rep.Jobs.Skipped++
			continue
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}

		quality := vid.quality
		if quality == "" {
			// download_jobs.quality is NOT NULL; the old requested default is
			// the honest fallback when the old job recorded no tier at all.
			quality = "highest"
		}
		if _, err := bw.exec(ref.id, ref.creator, quality, attempts,
			vid.size, vid.size, created, started, finished); err != nil {
			return err
		}
		m.rep.Jobs.Migrated++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return bw.finish()
}

// ------------------------------------------------------------ file links ---

// linkFiles wires the legacy corpus into the new data dir: junction by
// default, a real copy with -copy-files. Failures are warnings, never fatal.
func (m *migrator) linkFiles() error {
	if m.opts.CopyFiles {
		return m.copyAll()
	}
	return m.ensureJunction()
}

func (m *migrator) ensureJunction() error {
	if st, err := os.Stat(m.opts.LegacyRoot); err != nil || !st.IsDir() {
		m.rep.Junction = fmt.Sprintf("legacy downloads root missing: %s (no junction created)", m.opts.LegacyRoot)
		return nil
	}
	downloads := filepath.Join(m.opts.DataDir, "downloads")
	if err := os.MkdirAll(downloads, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", downloads, err)
	}
	link := filepath.Join(downloads, junctionName)
	target, err := filepath.Abs(m.opts.LegacyRoot)
	if err != nil {
		target = m.opts.LegacyRoot
	}

	if fi, err := os.Lstat(link); err == nil {
		if fi.IsDir() && fi.Mode()&os.ModeSymlink == 0 {
			m.rep.Junction = fmt.Sprintf("CONFLICT: %s exists as a real directory; junction not created", link)
			return nil
		}
		m.rep.Junction = fmt.Sprintf("already present: %s -> %s (skipped)", link, target)
		return nil
	}

	if runtime.GOOS != "windows" {
		// Development convenience: plain directory symlink.
		if err := os.Symlink(target, link); err != nil {
			return fmt.Errorf("symlink %s -> %s: %w", link, target, err)
		}
		m.rep.Junction = fmt.Sprintf("created symlink %s -> %s", link, target)
		return nil
	}

	// mklink is a cmd builtin; junctions need no administrator rights.
	cmd := exec.Command("cmd", "/c", "mklink", "/J", link, target)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mklink /J %s -> %s: %v: %s", link, target, err, strings.TrimSpace(string(out)))
	}
	m.rep.Junction = fmt.Sprintf("created %s -> %s", link, target)
	return nil
}

func (m *migrator) copyAll() error {
	if len(m.copies) == 0 {
		m.rep.Junction = "copy mode: nothing to copy (everything already migrated)"
		return nil
	}
	// If a previous junction-mode run left a link where the copies go, remove
	// the link itself (os.Remove on a junction touches the link only).
	link := filepath.Join(m.opts.DataDir, "downloads", junctionName)
	if fi, err := os.Lstat(link); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if err := os.Remove(link); err != nil {
			return fmt.Errorf("remove junction %s for copy mode: %w", link, err)
		}
		log.Printf("[import-legacy] removed junction %s (copy mode)", link)
	}
	for _, c := range m.copies {
		if err := m.ctx.Err(); err != nil {
			return err
		}
		if fi, err := os.Stat(c.dst); err == nil && fi.Size() > 0 {
			m.rep.FilesCopied++ // already in place from a previous run
			continue
		}
		if err := os.MkdirAll(filepath.Dir(c.dst), 0o755); err != nil {
			return fmt.Errorf("create %s: %w", filepath.Dir(c.dst), err)
		}
		if err := copyFile(c.src, c.dst); err != nil {
			m.warn("copy %s: %v", c.src, err)
			continue
		}
		m.rep.FilesCopied++
	}
	m.rep.Junction = fmt.Sprintf("copy mode: %d file(s) in place under %s", m.rep.FilesCopied, filepath.Join(m.opts.DataDir, "downloads"))
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// ---------------------------------------------------------------- totals ---

func (m *migrator) collectTotals() error {
	m.rep.Totals = map[string]int{}
	for _, t := range []string{"creators", "collections", "works", "assets", "download_jobs"} {
		var n int
		if err := m.target.QueryRow("SELECT COUNT(*) FROM " + t).Scan(&n); err != nil {
			return fmt.Errorf("count %s: %w", t, err)
		}
		m.rep.Totals[t] = n
	}
	return nil
}

// ----------------------------------------------------------- batch writer ---

// batchWriter batches INSERTs into transactions of size n (prepared
// statement, progress log per commit). Individual rows get their idempotency
// SELECT against the target before landing here.
type batchWriter struct {
	ctx   context.Context
	h     *sql.DB
	size  int
	query string
	name  string

	tx    *sql.Tx
	stmt  *sql.Stmt
	pend  int
	total int
}

func newBatchWriter(ctx context.Context, h *sql.DB, size int, query, name string) *batchWriter {
	return &batchWriter{ctx: ctx, h: h, size: size, query: query, name: name}
}

// exec buffers one INSERT; it commits whenever the batch is full.
func (b *batchWriter) exec(args ...any) (sql.Result, error) {
	if b.stmt == nil {
		if err := b.begin(); err != nil {
			return nil, err
		}
	}
	res, err := b.stmt.ExecContext(b.ctx, args...)
	if err != nil {
		return nil, err
	}
	b.pend++
	b.total++
	if b.pend >= b.size {
		if err := b.finish(); err != nil {
			return nil, err
		}
	}
	return res, nil
}

func (b *batchWriter) begin() error {
	tx, err := b.h.BeginTx(b.ctx, nil)
	if err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(b.ctx, b.query)
	if err != nil {
		tx.Rollback()
		return err
	}
	b.tx, b.stmt = tx, stmt
	return nil
}

// finish commits the open batch (if any) and logs progress.
func (b *batchWriter) finish() error {
	if b.stmt == nil {
		return nil
	}
	stmt, tx := b.stmt, b.tx
	b.stmt, b.tx, b.pend = nil, nil, 0
	if err := stmt.Close(); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if b.name != "" {
		log.Printf("[import-legacy] %s: %d row(s) committed", b.name, b.total)
	}
	return nil
}

// rollbackIfOpen aborts an unfinished batch after an error; the deferred call
// in each migrator keeps failed runs from committing partial batches.
func (b *batchWriter) rollbackIfOpen() {
	if b.tx == nil {
		return
	}
	_ = b.tx.Rollback()
	b.stmt, b.tx, b.pend = nil, nil, 0
}

// ----------------------------------------------------------------- output ---

func printReport(opts Options, rep Report, took time.Duration) {
	line := func(format string, args ...any) { fmt.Printf(format+"\n", args...) }
	line("== import-legacy report ==")
	line("legacy db    : %s (read-only)", opts.LegacyDB)
	line("new db       : %s", opts.DBPath)
	line("data dir     : %s", opts.DataDir)
	line("creators     : migrated %d, skipped %d", rep.Creators.Migrated, rep.Creators.Skipped)
	line("collections  : migrated %d, skipped %d", rep.Collections.Migrated, rep.Collections.Skipped)
	line("works        : migrated %d, skipped %d", rep.Works.Migrated, rep.Works.Skipped)
	line("assets       : migrated %d, skipped %d, missing video files %d", rep.Assets.Migrated, rep.Assets.Skipped, rep.MissingFiles)
	line("download_jobs: migrated %d, skipped %d (no validated video %d, other status %d)",
		rep.Jobs.Migrated, rep.Jobs.Skipped, rep.SkippedNoVid, rep.SkippedOther)
	if opts.CopyFiles {
		line("files        : copied %d", rep.FilesCopied)
	}
	line("linkage      : %s", rep.Junction)
	if len(rep.Warnings) > 0 {
		line("warnings     : %d", len(rep.Warnings))
		for _, w := range rep.Warnings {
			line("  - %s", w)
		}
	} else {
		line("warnings     : 0")
	}
	if rep.Totals != nil {
		line("new db totals: creators=%d collections=%d works=%d assets=%d download_jobs=%d",
			rep.Totals["creators"], rep.Totals["collections"], rep.Totals["works"],
			rep.Totals["assets"], rep.Totals["download_jobs"])
	}
	line("done in %s", took.Round(time.Millisecond))
}
