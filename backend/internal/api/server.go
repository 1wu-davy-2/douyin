// Package api wires the HTTP surface: routing, auth guard, the five auth
// endpoints, health, settings, the SSE event stream and the embedded SPA.
package api

import (
	"database/sql"
	"douyin/backend/internal/auth"
	"douyin/backend/internal/config"
	"douyin/backend/internal/downloader"
	"douyin/backend/internal/events"
	"douyin/backend/internal/provider"
	"douyin/backend/internal/scanner"
	"douyin/backend/internal/settings"
	"douyin/backend/internal/sidecar"
	"douyin/backend/internal/uploader"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"
)

// Deps carries everything the handlers need.
type Deps struct {
	Cfg        config.Settings
	Auth       *auth.Service
	Bus        *events.Bus
	Manager    *sidecar.Manager
	Store      *settings.Store
	Resolver   *provider.Resolver
	DB         *sql.DB
	Scanner    *scanner.Scanner
	Downloader *downloader.Downloader
	// Uploader is the MinIO sync service (may be nil: not wired / tests).
	// Nil handlers answer the test/status endpoints with ok=false / zeros.
	Uploader *uploader.Uploader
}

// Server is the HTTP application.
type Server struct {
	deps Deps
}

// New assembles the server.
func New(deps Deps) *Server {
	return &Server{deps: deps}
}

// Handler builds the root handler:
//
//	/api/auth/* + /api/health      -> public
//	/api/*                          -> authenticated (401 {"detail":"unauthorized"} otherwise)
//	anything else                   -> embedded SPA with index.html fallback
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// -- Stage 1 (implemented) ------------------------------------------------
	s.registerAuthRoutes(mux)     // auth.go
	s.registerHealthRoutes(mux)   // health.go
	s.registerSettingsRoutes(mux) // settings.go
	s.registerEventRoutes(mux)    // events.go

	// -- Stage 4 (implemented) ------------------------------------------------
	s.registerCreatorRoutes(mux)      // creators.go (works + collections included)
	s.registerSubscriptionRoutes(mux) // subscriptions.go

	// -- Stage 5 (implemented) ------------------------------------------------
	s.registerDownloadRoutes(mux) // downloads.go
	s.registerAssetRoutes(mux)    // assets.go

	// -- v1.3: per-creator download roots ------------------------------------
	s.registerCreatorRootRoutes(mux) // creator_roots.go

	// Fake CDN for mock mode: the mock provider hands out relative
	// /mockcdn/... URLs that the downloader resolves against this server.
	if s.deps.Cfg.Mock {
		s.registerMockCDN(mux) // mockcdn.go
	}

	// Every unregistered /api path still passes the auth guard below, so it
	// answers 401 when unauthenticated instead of leaking a 404.

	apiHandler := s.authGuard(mux)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/api" {
			apiHandler.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/mockcdn/") {
			// Fake CDN (mock mode only; route not registered otherwise).
			s.serveMockCDN(w, r)
			return
		}
		s.serveStatic(w, r)
	})
}

// authGuard enforces the session cookie on every /api path except the public
// exemptions (/api/auth/*, /api/health).
func (s *Server) authGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/api/health" || strings.HasPrefix(p, "/api/auth/") {
			next.ServeHTTP(w, r)
			return
		}
		s.deps.Auth.Middleware(next).ServeHTTP(w, r)
	})
}

// ------------------------------------------------------------------ helpers --

// nowRFC3339 renders the current UTC time in the contract's wire format.
func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// writeJSON emits a JSON response.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// writeError emits the contract's error shape: {"detail": "..."}.
func writeError(w http.ResponseWriter, status int, detail string) {
	writeJSON(w, status, map[string]string{"detail": detail})
}

// writeInternalError logs the cause and answers a generic 500 (never leaks
// internals to the client).
func writeInternalError(w http.ResponseWriter, err error) {
	log.Printf("[api] internal error: %v", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

// decodeJSON parses a bounded JSON request body into out.
func decodeJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(out); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// methodNotAllowed answers 405 for wrong-method matches on registered paths.
func methodNotAllowed(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusMethodNotAllowed, "method not allowed")
}

// endpoint is a tiny helper binding one method to one handler.
type endpoint struct {
	method string
	path   string
	fn     http.HandlerFunc
}

func register(mux *http.ServeMux, endpoints []endpoint) {
	for _, e := range endpoints {
		mux.HandleFunc(e.method+" "+e.path, e.fn)
	}
}
