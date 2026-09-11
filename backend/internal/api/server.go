// Package api wires the HTTP surface: routing, auth guard, the five auth
// endpoints, health, settings, the SSE event stream and the embedded SPA.
package api

import (
	"douyin/backend/internal/auth"
	"douyin/backend/internal/config"
	"douyin/backend/internal/events"
	"douyin/backend/internal/provider"
	"douyin/backend/internal/settings"
	"douyin/backend/internal/sidecar"
	"encoding/json"
	"log"
	"net/http"
	"strings"
)

// Deps carries everything the handlers need.
type Deps struct {
	Cfg      config.Settings
	Auth     *auth.Service
	Bus      *events.Bus
	Manager  *sidecar.Manager
	Store    *settings.Store
	Resolver *provider.Resolver
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

	// -- Later stages (registered by the corresponding files) -----------------
	// creators.go, downloads.go, subscriptions.go, assets.go currently hold no
	// routes; every unregistered /api path still passes the auth guard below,
	// so it answers 401 when unauthenticated instead of leaking a 404.

	apiHandler := s.authGuard(mux)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/api" {
			apiHandler.ServeHTTP(w, r)
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
