package api

// Embedded SPA static serving.
//
// The frontend build output is embedded from internal/api/dist (mirrored from
// frontend/dist by the build step — see .gitignore here; Go cannot embed
// files outside its module). Every non-/api path that does not match a real
// file falls back to index.html so client-side routes work.

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var distFS embed.FS

// distRoot strips the embed prefix so paths resolve inside dist/.
var (
	distRoot, _ = fs.Sub(distFS, "dist")
	indexHTML   = mustReadIndex(distRoot)
)

func mustReadIndex(root fs.FS) []byte {
	raw, err := fs.ReadFile(root, "index.html")
	if err != nil {
		// The placeholder index.html is committed, so this is unreachable in
		// practice; a hard panic would take the whole server down on a
		// packaging mistake, so keep a benign fallback instead.
		return []byte("<!doctype html><title>douyin archive</title>")
	}
	return raw
}

// contentTypeByExt maps the SPA's asset extensions explicitly: the system
// mime table (Windows registry!) is unreliable for .js/.css.
func contentTypeByExt(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".json", ".map":
		return "application/json"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".ico":
		return "image/x-icon"
	case ".woff":
		return "font/woff"
	case ".woff2":
		return "font/woff2"
	default:
		return mime.TypeByExtension(path.Ext(name))
	}
}

// serveStatic answers every non-/api request: the exact file if it exists in
// the embedded dist, otherwise index.html (SPA routing fallback).
func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request) {
	clean := path.Clean("/" + r.URL.Path) // e.g. /assets/app.js
	rel := strings.TrimPrefix(clean, "/")
	if rel == "" {
		rel = "index.html"
	}

	if data, err := fs.ReadFile(distRoot, rel); err == nil {
		if ct := contentTypeByExt(rel); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(indexHTML)
}
