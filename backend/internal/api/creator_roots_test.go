package api

// Endpoint tests for the v1.3 download-root surface:
//
//	PATCH /api/creators/{id}/download-root  (set / clear / validate)
//	POST  /api/creators/{id}/move-downloads (relocate, legacy skip, 409)
//	GET   /api/assets/{id}/content          (absolute + historical relative)
//
// plus unit tests for the path helpers backing the move endpoint.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func moveTestServer(t *testing.T) (*httptest.Server, *http.Client, *sql.DB, string) {
	t.Helper()
	server, _, database, cfg := newTestServerWithCfg(t)
	return server, setupAdmin(t, server), database, cfg.DataDir
}

func seedMoveCreator(t *testing.T, database *sql.DB, sec, nickname string) int64 {
	t.Helper()
	res, err := database.Exec(
		`INSERT INTO creators (sec_uid, nickname, profile_url, created_at) VALUES (?, ?, 'https://www.douyin.com/user/'||?, ?)`,
		sec, nickname, sec, nowRFC3339())
	if err != nil {
		t.Fatalf("insert creator: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func seedMoveWork(t *testing.T, database *sql.DB, creatorID int64, itemID string) int64 {
	t.Helper()
	res, err := database.Exec(
		`INSERT INTO works (creator_id, item_id, title, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		creatorID, itemID, "作品 "+itemID, nowRFC3339(), nowRFC3339())
	if err != nil {
		t.Fatalf("insert work: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func seedMoveAsset(t *testing.T, database *sql.DB, workID int64, kind, path string, size int64, quality any) int64 {
	t.Helper()
	res, err := database.Exec(
		`INSERT INTO assets (work_id, kind, path, size_bytes, quality, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		workID, kind, path, size, quality, nowRFC3339())
	if err != nil {
		t.Fatalf("insert asset %s: %v", path, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------- download-root --

func TestCreatorDownloadRootEndpoint(t *testing.T) {
	server, client, database, dataDir := moveTestServer(t)
	creatorID := seedMoveCreator(t, database, "MS4wLjABAAAAroot1", "根博主")
	base := fmt.Sprintf("%s/api/creators/%d/download-root", server.URL, creatorID)

	// Set a valid absolute override: echoed back, stored, created on disk.
	override := filepath.Join(t.TempDir(), "creator-media")
	status, raw := patchJSON(t, client, base, `{"path":`+jsonQuote(override)+`}`)
	if status != http.StatusOK {
		t.Fatalf("set status = %d body = %s", status, raw)
	}
	var body struct {
		OK           bool   `json:"ok"`
		DownloadRoot string `json:"download_root"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, raw)
	}
	if !body.OK || body.DownloadRoot != filepath.Clean(override) {
		t.Fatalf("response = %s, want ok with download_root %q", raw, filepath.Clean(override))
	}
	if fi, err := os.Stat(override); err != nil || !fi.IsDir() {
		t.Fatalf("override dir not created: %v", err)
	}
	var stored sql.NullString
	if err := database.QueryRow(`SELECT download_root FROM creators WHERE id = ?`, creatorID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !stored.Valid || stored.String != filepath.Clean(override) {
		t.Fatalf("stored override = %v, want %q", stored, filepath.Clean(override))
	}

	// A relative path is rejected.
	status, raw = patchJSON(t, client, base, `{"path":"relative/dir"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("relative path status = %d body = %s", status, raw)
	}

	// null clears the override: the response reports the effective root
	// (default <data_dir>/downloads) and the row goes back to NULL.
	status, raw = patchJSON(t, client, base, `{"path":null}`)
	if status != http.StatusOK {
		t.Fatalf("clear status = %d body = %s", status, raw)
	}
	body = struct {
		OK           bool   `json:"ok"`
		DownloadRoot string `json:"download_root"`
	}{}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, raw)
	}
	if !body.OK || body.DownloadRoot != filepath.Join(dataDir, "downloads") {
		t.Fatalf("cleared response = %s, want default root %q", raw, filepath.Join(dataDir, "downloads"))
	}
	if err := database.QueryRow(`SELECT download_root FROM creators WHERE id = ?`, creatorID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored.Valid {
		t.Fatalf("stored override after clear = %q, want NULL", stored.String)
	}

	// Unknown creator -> 404.
	status, _ = patchJSON(t, client, server.URL+"/api/creators/999/download-root", `{"path":null}`)
	if status != http.StatusNotFound {
		t.Fatalf("unknown creator status = %d, want 404", status)
	}
}

func jsonQuote(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

// ---------------------------------------------------------- move-downloads --

func TestCreatorMoveDownloadsRelocatesFiles(t *testing.T) {
	server, client, database, dataDir := moveTestServer(t)
	creatorID := seedMoveCreator(t, database, "MS4wLjABAAAAmove1", "搬家博主")

	// Old layout: two video works plus metadata under the default root, and
	// one cover asset through the legacy junction (relative path).
	bucket := filepath.Join(dataDir, "downloads", "搬家博主_MS4wLjABAAAAmove1", "singles")
	w1 := seedMoveWork(t, database, creatorID, "move_1")
	w2 := seedMoveWork(t, database, creatorID, "move_2")
	fileA := filepath.Join(bucket, "a.mp4")
	fileMeta := filepath.Join(bucket, "a.metadata.json")
	fileB := filepath.Join(bucket, "b.mp4")
	fileLegacy := filepath.Join(dataDir, "downloads", "legacy", "keep.jpg")
	mustWriteFile(t, fileA, strings.Repeat("A", 100))
	mustWriteFile(t, fileMeta, "{}")
	mustWriteFile(t, fileB, strings.Repeat("B", 200))
	mustWriteFile(t, fileLegacy, strings.Repeat("L", 77))
	seedMoveAsset(t, database, w1, "video", filepath.ToSlash(fileA), 100, "1080p")
	seedMoveAsset(t, database, w1, "metadata", filepath.ToSlash(fileMeta), 2, nil)
	seedMoveAsset(t, database, w2, "video", filepath.ToSlash(fileB), 200, "1080p")
	seedMoveAsset(t, database, w1, "cover", "downloads/legacy/keep.jpg", 77, nil)

	target := filepath.Join(t.TempDir(), "new-home")
	status, raw := postJSON(t, client,
		fmt.Sprintf("%s/api/creators/%d/move-downloads", server.URL, creatorID),
		`{"target_root":`+jsonQuote(target)+`}`)
	if status != http.StatusOK {
		t.Fatalf("move status = %d body = %s", status, raw)
	}
	var res struct {
		MovedFiles   int                 `json:"moved_files"`
		MovedBytes   int64               `json:"moved_bytes"`
		SkippedFiles int                 `json:"skipped_files"`
		FailedFiles  []map[string]string `json:"failed_files"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("decode: %v (%s)", err, raw)
	}
	if res.MovedFiles != 3 || res.MovedBytes != 302 || res.SkippedFiles != 1 || len(res.FailedFiles) != 0 {
		t.Fatalf("move result = %+v, want 3 moved / 302 bytes / 1 skipped / no failures", res)
	}

	// Files relocated with the relative structure preserved...
	newA := filepath.Join(target, "搬家博主_MS4wLjABAAAAmove1", "singles", "a.mp4")
	newMeta := filepath.Join(target, "搬家博主_MS4wLjABAAAAmove1", "singles", "a.metadata.json")
	newB := filepath.Join(target, "搬家博主_MS4wLjABAAAAmove1", "singles", "b.mp4")
	for _, p := range []string{newA, newMeta, newB} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("relocated file %s missing: %v", p, err)
		}
	}
	for _, p := range []string{fileA, fileMeta, fileB} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("old file %s still present (stat = %v)", p, err)
		}
	}
	// ...the legacy junction file untouched (path and disk)...
	if _, err := os.Stat(fileLegacy); err != nil {
		t.Fatalf("legacy file must stay: %v", err)
	}
	var legacyPath string
	if err := database.QueryRow(
		`SELECT path FROM assets WHERE work_id = ? AND kind = 'cover'`, w1).Scan(&legacyPath); err != nil {
		t.Fatal(err)
	}
	if legacyPath != "downloads/legacy/keep.jpg" {
		t.Fatalf("legacy asset path = %q, want untouched relative path", legacyPath)
	}
	// ...rows rewritten to absolute forward-slash paths under the target...
	wantPrefix := filepath.ToSlash(target) + "/搬家博主_MS4wLjABAAAAmove1/singles/"
	var gotA, gotMeta, gotB string
	if err := database.QueryRow(`SELECT path FROM assets WHERE work_id = ? AND kind = 'video' AND quality = '1080p'`, w1).Scan(&gotA); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT path FROM assets WHERE work_id = ? AND kind = 'metadata'`, w1).Scan(&gotMeta); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT path FROM assets WHERE work_id = ? AND kind = 'video'`, w2).Scan(&gotB); err != nil {
		t.Fatal(err)
	}
	for _, got := range []string{gotA, gotMeta, gotB} {
		if !strings.HasPrefix(got, wantPrefix) || strings.Contains(got, `\`) {
			t.Fatalf("moved asset path = %q, want prefix %q", got, wantPrefix)
		}
	}
	// ...and the vacated creator bucket pruned (the root itself stays).
	if _, err := os.Stat(filepath.Join(dataDir, "downloads", "搬家博主_MS4wLjABAAAAmove1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old creator bucket still present (stat = %v)", err)
	}
	if fi, err := os.Stat(filepath.Join(dataDir, "downloads")); err != nil || !fi.IsDir() {
		t.Fatalf("old downloads root must survive: %v", err)
	}
}

func TestCreatorMoveDownloadsRefusesWhileDownloading(t *testing.T) {
	server, client, database, _ := moveTestServer(t)
	creatorID := seedMoveCreator(t, database, "MS4wLjABAAAAbusy1", "忙碌博主")
	workID := seedMoveWork(t, database, creatorID, "busy_1")
	if _, err := database.Exec(
		`INSERT INTO download_jobs (work_id, creator_id, quality, status, attempts, queued_at)
		 VALUES (?, ?, '1080p', 'downloading', 1, ?)`,
		workID, creatorID, nowRFC3339()); err != nil {
		t.Fatal(err)
	}

	status, raw := postJSON(t, client,
		fmt.Sprintf("%s/api/creators/%d/move-downloads", server.URL, creatorID),
		`{"target_root":`+jsonQuote(t.TempDir())+`}`)
	if status != http.StatusConflict {
		t.Fatalf("status = %d body = %s, want 409 while a job is downloading", status, raw)
	}
}

func TestCreatorMoveDownloadsValidation(t *testing.T) {
	server, client, database, _ := moveTestServer(t)
	creatorID := seedMoveCreator(t, database, "MS4wLjABAAAAvalid1", "校验博主")

	// A relative target is rejected.
	status, _ := postJSON(t, client,
		fmt.Sprintf("%s/api/creators/%d/move-downloads", server.URL, creatorID),
		`{"target_root":"relative/target"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("relative target status = %d, want 400", status)
	}
	// An empty target is rejected.
	status, _ = postJSON(t, client,
		fmt.Sprintf("%s/api/creators/%d/move-downloads", server.URL, creatorID),
		`{"target_root":""}`)
	if status != http.StatusBadRequest {
		t.Fatalf("empty target status = %d, want 400", status)
	}
	// Unknown creator -> 404.
	status, _ = postJSON(t, client, server.URL+"/api/creators/999/move-downloads",
		`{"target_root":`+jsonQuote(t.TempDir())+`}`)
	if status != http.StatusNotFound {
		t.Fatalf("unknown creator status = %d, want 404", status)
	}
}

// ----------------------------------------------------------------- helpers --

func TestMoveFileFallsBackToCopyAcrossVolumes(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	dst := filepath.Join(dir, "dst", "src.bin")
	mustWriteFile(t, src, strings.Repeat("X", 1000))

	// Force the cross-volume path (rename fails, e.g. E: -> F:): the copy
	// fallback must land a byte-identical file and remove the source.
	orig := renameForMove
	renameForMove = func(_, _ string) error { return errors.New("move across volumes is not supported") }
	t.Cleanup(func() { renameForMove = orig })

	if err := moveFile(src, dst); err != nil {
		t.Fatalf("moveFile: %v", err)
	}
	raw, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if len(raw) != 1000 {
		t.Fatalf("dst size = %d, want 1000 (size verified)", len(raw))
	}
	if _, err := os.Stat(src); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source must be removed after a verified copy (stat = %v)", err)
	}
}

func TestStripKnownRootsLegacyAndPriority(t *testing.T) {
	defaultRoot := `E:\data\downloads`
	creatorRoot := `E:\data\downloads\creatorA` // nested inside the default root
	roots := []string{creatorRoot, defaultRoot}

	// A file under the creator root strips against the LONGEST root.
	rel, matched, ok, legacy := stripKnownRoots(`E:\data\downloads\creatorA\singles\v.mp4`, roots)
	if !ok || legacy || rel != "singles/v.mp4" || matched != creatorRoot {
		t.Fatalf("strip = %q/%q/%v/%v, want singles/v.mp4 vs creatorRoot", rel, matched, ok, legacy)
	}
	// A default-root file keeps the creator bucket as its structure.
	rel, _, ok, legacy = stripKnownRoots(`E:\data\downloads\creatorB\singles\v.mp4`, roots)
	if !ok || legacy || rel != "creatorB/singles/v.mp4" {
		t.Fatalf("strip = %q/%v/%v, want creatorB/singles/v.mp4", rel, ok, legacy)
	}
	// Legacy junction content is flagged and never moved.
	rel, _, ok, legacy = stripKnownRoots(`E:\data\downloads\legacy\archive\v.mp4`, roots)
	if !ok || !legacy || rel != "legacy/archive/v.mp4" {
		t.Fatalf("strip = %q/%v/%v, want legacy detection", rel, ok, legacy)
	}
	// An unrelated absolute path matches nothing.
	if _, _, ok, _ := stripKnownRoots(`F:\elsewhere\v.mp4`, roots); ok {
		t.Fatal("unrelated path must not strip")
	}
}

// ---------------------------------------------------------- asset content --

func TestAssetContentResolvesAbsoluteAndRelativePaths(t *testing.T) {
	server, client, database, dataDir := moveTestServer(t)

	// Absolute stored path (v1.3 shape): served verbatim.
	absFile := filepath.Join(t.TempDir(), "abs.bin")
	mustWriteFile(t, absFile, "ABS-CONTENT")
	w1 := seedMoveWork(t, database, seedMoveCreator(t, database, "MS4wLjABAAAAcontent1", "内容博主"), "content_1")
	idAbs := seedMoveAsset(t, database, w1, "video", filepath.ToSlash(absFile), 11, "1080p")

	// Relative stored path (historical shape): resolved against the data dir.
	relFile := filepath.Join(dataDir, "downloads", "rel", "relative.txt")
	mustWriteFile(t, relFile, "REL-CONTENT")
	idRel := seedMoveAsset(t, database, w1, "metadata", "downloads/rel/relative.txt", 11, nil)

	for _, tc := range []struct {
		id   int64
		want string
	}{
		{idAbs, "ABS-CONTENT"},
		{idRel, "REL-CONTENT"},
	} {
		resp, err := client.Get(fmt.Sprintf("%s/api/assets/%d/content", server.URL, tc.id))
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(raw) != tc.want {
			t.Fatalf("asset %d content = %d %q, want 200 %q", tc.id, resp.StatusCode, raw, tc.want)
		}
	}

	// A missing file answers 404, not 500.
	ghost := seedMoveAsset(t, database, w1, "cover", filepath.ToSlash(filepath.Join(t.TempDir(), "ghost.jpg")), 0, nil)
	resp, err := client.Get(fmt.Sprintf("%s/api/assets/%d/content", server.URL, ghost))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing file status = %d, want 404", resp.StatusCode)
	}
}
