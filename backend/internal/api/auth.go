package api

// Auth endpoints (docs/api.md "认证 Auth") — all five are public paths, but
// /api/auth/password additionally requires a valid session.

import (
	"errors"
	"net"
	"net/http"
	"strings"

	"douyin/backend/internal/auth"
)

func (s *Server) registerAuthRoutes(mux *http.ServeMux) {
	register(mux, []endpoint{
		{http.MethodGet, "/api/auth/status", s.handleAuthStatus},
		{http.MethodPost, "/api/auth/setup", s.handleAuthSetup},
		{http.MethodPost, "/api/auth/login", s.handleAuthLogin},
		{http.MethodPost, "/api/auth/logout", s.handleAuthLogout},
		{http.MethodPost, "/api/auth/password", s.deps.Auth.Middleware(http.HandlerFunc(s.handleAuthPassword)).ServeHTTP},
	})
}

// authStatus GET /api/auth/status ->
// {authenticated, initialized, username?}
//
// This path is public (exempt from the auth guard), so the middleware never
// injected the username; the cookie is validated here directly.
func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	username := auth.UsernameFrom(r.Context())
	if username == "" {
		if cookie, err := r.Cookie(auth.CookieName); err == nil {
			if name, ok := s.deps.Auth.Validate(r.Context(), cookie.Value); ok {
				username = name
				// Keep the sliding expiry consistent with guarded endpoints.
				auth.SetSessionCookie(w, cookie.Value)
			}
		}
	}
	resp := struct {
		Authenticated bool   `json:"authenticated"`
		Initialized   bool   `json:"initialized"`
		Username      string `json:"username,omitempty"`
	}{
		Authenticated: username != "",
		Initialized:   s.deps.Auth.Initialized(r.Context()),
	}
	if username != "" {
		resp.Username = username
	}
	writeJSON(w, http.StatusOK, resp)
}

// authSetup POST /api/auth/setup — first-time admin bootstrap only.
func (s *Server) handleAuthSetup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := s.deps.Auth.Setup(r.Context(), body.Username, body.Password); err != nil {
		switch {
		case errors.Is(err, auth.ErrAlreadyInitialized):
			writeError(w, http.StatusConflict, "admin already initialized")
		default:
			writeError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	token, err := s.deps.Auth.CreateSession(r.Context(), strings.TrimSpace(body.Username))
	if err != nil {
		writeInternalError(w, err)
		return
	}
	auth.SetSessionCookie(w, token)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "username": strings.TrimSpace(body.Username)})
}

// clientIP extracts the remote address for the login rate limiter.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// authLogin POST /api/auth/login -> {ok, username}; rate-limited per IP.
func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	token, err := s.deps.Auth.Login(r.Context(), clientIP(r), body.Username, body.Password)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrRateLimited):
			writeError(w, http.StatusTooManyRequests, "too many failed attempts, locked for 15 minutes")
		case errors.Is(err, auth.ErrInvalidCredentials):
			writeError(w, http.StatusUnauthorized, "invalid credentials")
		case errors.Is(err, auth.ErrNotInitialized):
			writeError(w, http.StatusBadRequest, "admin not initialized yet")
		default:
			writeInternalError(w, err)
		}
		return
	}
	auth.SetSessionCookie(w, token)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "username": strings.TrimSpace(body.Username)})
}

// authLogout POST /api/auth/logout — clears the session (idempotent).
func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(auth.CookieName); err == nil {
		if err := s.deps.Auth.Logout(r.Context(), cookie.Value); err != nil {
			writeInternalError(w, err)
			return
		}
	}
	auth.ClearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// authPassword POST /api/auth/password {old_password, new_password} (authed).
func (s *Server) handleAuthPassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	username := auth.UsernameFrom(r.Context())
	if err := s.deps.Auth.ChangePassword(r.Context(), username, body.OldPassword, body.NewPassword); err != nil {
		switch {
		case errors.Is(err, auth.ErrWrongOldPassword):
			writeError(w, http.StatusBadRequest, "old password incorrect")
		default:
			writeError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
