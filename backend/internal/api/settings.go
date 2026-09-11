package api

// Settings endpoints: GET/PATCH /api/settings and the SMTP test notification.

import (
	"net/http"

	"douyin/backend/internal/settings"
)

func (s *Server) registerSettingsRoutes(mux *http.ServeMux) {
	register(mux, []endpoint{
		{http.MethodGet, "/api/settings", s.handleSettingsGet},
		{http.MethodPatch, "/api/settings", s.handleSettingsPatch},
		{http.MethodPost, "/api/notifications/test", s.handleNotificationsTest},
	})
}

// handleSettingsGet GET /api/settings -> full view with masked cookie and no
// SMTP password.
func (s *Server) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	view, err := s.deps.Store.View(r.Context())
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// handleSettingsPatch PATCH /api/settings — partial update with validation.
// On a cookie change the .cookie file is rewritten for the sidecar; provider
// switches take effect on the next request (no process restart).
func (s *Server) handleSettingsPatch(w http.ResponseWriter, r *http.Request) {
	var patch settings.Patch
	if !decodeJSON(w, r, &patch) {
		return
	}
	if _, err := s.deps.Store.Apply(r.Context(), patch); err != nil {
		// Validation failures are client errors.
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	view, err := s.deps.Store.View(r.Context())
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// handleNotificationsTest POST /api/notifications/test -> {ok, error?}.
// Always answers 200; success/failure is carried in the body.
func (s *Server) handleNotificationsTest(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.Store.SMTPSettings(r.Context())
	if err := settings.SendTestMail(cfg); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
