package api

// Asset streaming endpoints (docs/api.md "资产与播放 Assets"). Replaces the
// stage-1 placeholder.
//
//	GET /api/works/{id}/assets   -> asset list (video -> image -> cover ->
//	                               metadata per the v1.1 ordering)
//	GET /api/assets/{id}/content -> stream the file with native Range support
//	                               (http.ServeContent)
//
// assets.path is stored relative to the data dir (forward slashes); the
// content endpoint resolves it back onto the local filesystem.

import (
	"database/sql"
	"errors"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

func (s *Server) registerAssetRoutes(mux *http.ServeMux) {
	register(mux, []endpoint{
		{http.MethodGet, "/api/works/{id}/assets", s.handleWorkAssets},
		{http.MethodGet, "/api/assets/{id}/content", s.handleAssetContent},
	})
}

// workAsset is the contract's asset object (work_id included).
type workAsset struct {
	ID        int64   `json:"id"`
	WorkID    int64   `json:"work_id"`
	Kind      string  `json:"kind"`
	Path      string  `json:"path"`
	SizeBytes int64   `json:"size_bytes"`
	Quality   *string `json:"quality"`
	CreatedAt string  `json:"created_at"`
}

// assetOrder is the contract ordering for every asset list
// (docs/api.md v1.1): video assets newest-first (a live-photo clip of an
// image work is a video asset too) -> image assets by their 4-digit quality
// sequence ascending -> cover -> metadata.
const assetOrder = `
ORDER BY CASE kind WHEN 'video' THEN 0 WHEN 'image' THEN 1 WHEN 'cover' THEN 2 ELSE 3 END,
         CASE WHEN kind = 'video' THEN -id ELSE 0 END,
         CASE WHEN kind = 'image' THEN COALESCE(quality, '') ELSE '' END`

// handleWorkAssets GET /api/works/{id}/assets — ordered per the contract.
func (s *Server) handleWorkAssets(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	rows, err := s.deps.DB.QueryContext(r.Context(), `
		SELECT id, work_id, kind, path, size_bytes, quality, created_at
		FROM assets WHERE work_id = ?
		`+assetOrder, id)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	defer rows.Close()

	out := make([]workAsset, 0)
	for rows.Next() {
		var a workAsset
		var quality sql.NullString
		if err := rows.Scan(&a.ID, &a.WorkID, &a.Kind, &a.Path, &a.SizeBytes, &quality, &a.CreatedAt); err != nil {
			writeInternalError(w, err)
			return
		}
		if quality.Valid {
			a.Quality = &quality.String
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// assetContentType maps the extensions the archiver produces; the Windows
// registry mime table is unreliable, so never rely on it for media types.
func assetContentType(name string, kind string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".mp4", ".mov", ".m4v":
		return "video/mp4"
	case ".json":
		return "application/json"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	}
	if ct := contentTypeByExt(name); ct != "" {
		return ct
	}
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// handleAssetContent GET /api/assets/{id}/content — streams the stored file
// through http.ServeContent (native Range / If-Modified-Since handling).
func (s *Server) handleAssetContent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var kind, storedPath string
	err := s.deps.DB.QueryRowContext(r.Context(),
		`SELECT kind, path FROM assets WHERE id = ?`, id).Scan(&kind, &storedPath)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "asset not found")
		return
	}
	if err != nil {
		writeInternalError(w, err)
		return
	}

	full := resolveAssetPath(s.deps.Cfg.DataDir, storedPath)
	f, err := os.Open(full)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			writeError(w, http.StatusNotFound, "asset file missing on disk")
			return
		}
		writeInternalError(w, err)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		writeInternalError(w, err)
		return
	}

	name := filepath.Base(full)
	w.Header().Set("Content-Type", assetContentType(name, kind))
	w.Header().Set("Accept-Ranges", "bytes")
	http.ServeContent(w, r, name, fi.ModTime(), f)
}

// resolveAssetPath joins the stored (relative, forward-slash) path onto the
// data dir; absolute stored paths pass through unchanged.
func resolveAssetPath(dataDir, stored string) string {
	if filepath.IsAbs(stored) {
		return filepath.Clean(stored)
	}
	return filepath.Join(dataDir, filepath.FromSlash(stored))
}
