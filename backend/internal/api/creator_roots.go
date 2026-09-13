package api

// Creator download-root endpoints (docs/api.md v1.3, "存储路径规则"):
//
//	PATCH /api/creators/{id}/download-root  -> per-creator root override
//	POST  /api/creators/{id}/move-downloads -> relocate downloaded files
//
// Root resolution everywhere: creators.download_root (when set) -> global
// settings.download_root (when set) -> <data_dir>/downloads.
//
// The move endpoint strips each asset's current root to compute the preserved
// relative structure ({creator}/{collections|singles}/...), relocates the file
// (rename; cross-volume falls back to copy + size check + remove), rewrites
// assets.path to the new absolute location and prunes directories the creator
// vacated. Files under the legacy junction (<data>/downloads/legacy) are
// never moved: that tree is the read-only archive of the v1 project.

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"douyin/backend/internal/downloader"
	"douyin/backend/internal/settings"
)

func (s *Server) registerCreatorRootRoutes(mux *http.ServeMux) {
	register(mux, []endpoint{
		{http.MethodPatch, "/api/creators/{id}/download-root", s.handleCreatorDownloadRoot},
		{http.MethodPost, "/api/creators/{id}/move-downloads", s.handleCreatorMoveDownloads},
	})
}

// ----------------------------------------------------------- download-root --

// handleCreatorDownloadRoot PATCH /api/creators/{id}/download-root
// {path: string|null}: null/empty clears the override (follow the global
// root), otherwise the path must be absolute and is created eagerly.
// -> {ok, download_root: <effective root>}.
func (s *Server) handleCreatorDownloadRoot(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Path *string `json:"path"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	// NULL (SQL) clears the override; a non-empty validated path sets it.
	var override any
	if body.Path != nil && strings.TrimSpace(*body.Path) != "" {
		root, err := settings.ValidateDownloadRoot(*body.Path)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		override = root
	}

	ctx := r.Context()
	res, err := s.deps.DB.ExecContext(ctx,
		`UPDATE creators SET download_root = ? WHERE id = ?`, override, id)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeError(w, http.StatusNotFound, "creator not found")
		return
	}
	effective, err := s.effectiveCreatorRoot(ctx, id)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "download_root": effective})
}

// effectiveCreatorRoot resolves the root that applies to one creator
// (contract v1.3 priority: override -> global setting -> default).
func (s *Server) effectiveCreatorRoot(ctx context.Context, creatorID int64) (string, error) {
	var override sql.NullString
	err := s.deps.DB.QueryRowContext(ctx,
		`SELECT download_root FROM creators WHERE id = ?`, creatorID).Scan(&override)
	if err != nil {
		return "", err
	}
	if override.Valid && strings.TrimSpace(override.String) != "" {
		return override.String, nil
	}
	return s.deps.Store.EffectiveDownloadRoot(ctx), nil
}

// ---------------------------------------------------------- move-downloads --

// moveFileFailure is one entry of the failed_files response list.
type moveFileFailure struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

// moveDownloadsResult is the POST /api/creators/{id}/move-downloads payload.
type moveDownloadsResult struct {
	MovedFiles   int               `json:"moved_files"`
	MovedBytes   int64             `json:"moved_bytes"`
	SkippedFiles int               `json:"skipped_files"`
	FailedFiles  []moveFileFailure `json:"failed_files"`
}

// renameForMove is swappable in tests (forces the cross-volume code path).
var renameForMove = os.Rename

// handleCreatorMoveDownloads POST /api/creators/{id}/move-downloads
// {target_root}: moves every downloaded file of the creator to the new root,
// preserving the relative structure. Refused with 409 while any job of this
// creator is downloading.
func (s *Server) handleCreatorMoveDownloads(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		TargetRoot string `json:"target_root"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	targetRoot, err := settings.ValidateDownloadRoot(body.TargetRoot)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if targetRoot == "" {
		writeError(w, http.StatusBadRequest, "target_root must be a non-empty absolute path")
		return
	}

	ctx := r.Context()
	if err := s.creatorExists(ctx, id); err != nil {
		writeCreatorExistsError(w, err)
		return
	}
	// A downloading job may still be writing into the tree we relocate:
	// refuse rather than race the worker (contract: 409).
	var active int
	if err := s.deps.DB.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM download_jobs WHERE creator_id = ? AND status = ?)`,
		id, downloader.StatusDownloading).Scan(&active); err != nil {
		writeInternalError(w, err)
		return
	}
	if active != 0 {
		writeError(w, http.StatusConflict, "creator has a downloading job, cancel it or wait for it to finish")
		return
	}

	// Root candidates for stripping the relative structure, in the contract
	// priority order; the longest match wins so a creator root nested inside
	// the default root still strips correctly.
	creatorRoot, err := s.creatorRootOverride(ctx, id)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	roots := []string{}
	if creatorRoot != "" {
		roots = append(roots, creatorRoot)
	}
	if global := s.deps.Store.DownloadRoot(ctx); global != "" {
		roots = append(roots, global)
	}
	// DataDir 可能是相对值(如默认 "./data"):构建根列表前必须转为绝对,
	// 否则与 assets.path 里的绝对路径做前缀匹配永远失败。
	dataDir, err := filepath.Abs(s.deps.Cfg.DataDir)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	roots = append(roots,
		filepath.Join(dataDir, "downloads"), // default root
		dataDir,                             // last resort (historical relative rows)
	)

	res := moveDownloadsResult{FailedFiles: []moveFileFailure{}}
	type cleanup struct{ dir, stop string }
	cleanups := []cleanup{}
	seenCleanup := map[string]bool{}

	rows, err := s.deps.DB.QueryContext(ctx, `
		SELECT a.id, a.path, a.size_bytes
		FROM assets a
		JOIN works w ON w.id = a.work_id
		WHERE w.creator_id = ?
		ORDER BY a.id`, id)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	type assetRow struct {
		id        int64
		path      string
		sizeBytes int64
	}
	assets := []assetRow{}
	for rows.Next() {
		var a assetRow
		if err := rows.Scan(&a.id, &a.path, &a.sizeBytes); err != nil {
			rows.Close()
			writeInternalError(w, err)
			return
		}
		assets = append(assets, a)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		writeInternalError(w, err)
		return
	}
	rows.Close()

	for _, a := range assets {
		full := resolveAssetPath(s.deps.Cfg.DataDir, a.path)
		rel, matched, ok, legacy := stripKnownRoots(full, roots)
		if legacy {
			// Read-only archive of the v1 project (junction): never touched.
			res.SkippedFiles++
			continue
		}
		if !ok || rel == "" {
			res.FailedFiles = append(res.FailedFiles, moveFileFailure{
				Path:  a.path,
				Error: "cannot determine the current download root for this path",
			})
			continue
		}
		dst := filepath.Join(targetRoot, filepath.FromSlash(rel))
		if sameFilePath(full, dst) {
			// Already at the target (e.g. move to the current root): nothing
			// to do on disk, the row update is a no-op.
			res.MovedFiles++
			res.MovedBytes += a.sizeBytes
			continue
		}
		if err := moveFile(full, dst); err != nil {
			res.FailedFiles = append(res.FailedFiles, moveFileFailure{Path: a.path, Error: err.Error()})
			continue
		}
		if _, err := s.deps.DB.ExecContext(ctx,
			`UPDATE assets SET path = ? WHERE id = ?`, filepath.ToSlash(filepath.Clean(dst)), a.id); err != nil {
			res.FailedFiles = append(res.FailedFiles, moveFileFailure{Path: a.path, Error: "moved on disk but db update failed: " + err.Error()})
			continue
		}
		res.MovedFiles++
		res.MovedBytes += a.sizeBytes
		key := matched + "\x00" + filepath.Dir(full)
		if !seenCleanup[key] {
			seenCleanup[key] = true
			cleanups = append(cleanups, cleanup{dir: filepath.Dir(full), stop: matched})
		}
	}

	// Prune directories the creator vacated (never the roots themselves).
	for _, c := range cleanups {
		removeEmptyDirs(c.dir, c.stop)
	}

	writeJSON(w, http.StatusOK, res)
}

// creatorRootOverride returns the raw creators.download_root value ("" when
// unset) — the stored override, not the effective root.
func (s *Server) creatorRootOverride(ctx context.Context, id int64) (string, error) {
	var override sql.NullString
	err := s.deps.DB.QueryRowContext(ctx,
		`SELECT download_root FROM creators WHERE id = ?`, id).Scan(&override)
	if err != nil {
		return "", err
	}
	if !override.Valid {
		return "", nil
	}
	return strings.TrimSpace(override.String), nil
}

// stripKnownRoots strips the longest matching root from full. Returns
// (rel, matchedRoot, ok, legacy); legacy marks files under the read-only
// v1 junction (first path segment "legacy" after stripping).
func stripKnownRoots(full string, roots []string) (string, string, bool, bool) {
	bestRel, bestRoot, bestLen := "", "", -1
	for _, root := range roots {
		if rel, ok := stripRoot(full, root); ok && len(root) > bestLen {
			bestRel, bestRoot, bestLen = rel, root, len(root)
		}
	}
	if bestLen < 0 {
		return "", "", false, false
	}
	if bestRel == "legacy" || strings.HasPrefix(bestRel, "legacy/") {
		return bestRel, bestRoot, true, true
	}
	return bestRel, bestRoot, true, false
}

// stripRoot removes root (cleaned) from full when full lives directly under
// it, returning the rest of the path with forward slashes. The prefix
// comparison is case-insensitive on Windows (NTFS is case-insensitive; the
// same root may be spelled with either case across settings/migration).
func stripRoot(full, root string) (string, bool) {
	if strings.TrimSpace(root) == "" {
		return "", false
	}
	f := filepath.ToSlash(filepath.Clean(full))
	p := strings.TrimSuffix(filepath.ToSlash(filepath.Clean(root)), "/")
	if p == "" || len(f) <= len(p) {
		return "", false
	}
	head, rest := f[:len(p)], f[len(p):]
	same := head == p
	if runtime.GOOS == "windows" {
		same = strings.EqualFold(head, p)
	}
	if !same || rest[0] != '/' {
		return "", false
	}
	return rest[1:], true
}

// sameFilePath compares two paths case-insensitively on Windows.
func sameFilePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// moveFile relocates one file. Same volume: a plain rename. Across volumes
// (the E: -> F: migration case, where os.Rename fails on Windows): copy,
// verify the byte count, then remove the source.
func moveFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("create target directory: %w", err)
	}
	if err := renameForMove(src, dst); err == nil {
		return nil
	}
	if err := copyVerified(src, dst); err != nil {
		return err
	}
	if err := os.Remove(src); err != nil {
		return fmt.Errorf("copied to target but could not remove the source: %w", err)
	}
	return nil
}

// copyVerified copies src to dst and refuses to report success unless the
// written byte count equals the source size; a partial destination is removed.
func copyVerified(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	n, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		_ = os.Remove(dst)
		return copyErr
	}
	if n != fi.Size() {
		_ = os.Remove(dst)
		return fmt.Errorf("size mismatch after copy: wrote %d of %d bytes", n, fi.Size())
	}
	return nil
}

// removeEmptyDirs deletes now-empty directories walking up from dir, stopping
// at stop (exclusive): the vacated creator bucket disappears, the root stays.
func removeEmptyDirs(dir, stop string) {
	dir = filepath.Clean(dir)
	stop = filepath.Clean(stop)
	for !sameFilePath(dir, stop) {
		parent := filepath.Dir(dir)
		if parent == dir {
			return // reached the volume root
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			return // vanished under us or still holding files
		}
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = parent
	}
}
