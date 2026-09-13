package api

// MinIO sync endpoints (docs/MINIO_PLAN.md): the settings-page connection
// test and the sync status counters. Both follow the notifications-test
// convention: always HTTP 200, success/failure carried in the body.

import (
	"net/http"

	"douyin/backend/internal/uploader"
)

// handleMinioTest POST /api/settings/minio/test -> {ok, error?}. Dials the
// currently configured endpoint and checks the bucket exists (the bucket is
// never created for the user).
func (s *Server) handleMinioTest(w http.ResponseWriter, r *http.Request) {
	if s.deps.Uploader == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "minio sync is not wired"})
		return
	}
	if err := s.deps.Uploader.TestConnection(r.Context()); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleMinioStatus GET /api/settings/minio/status -> queue counters
// (queued/uploaded/failed/dropped totals + last error).
func (s *Server) handleMinioStatus(w http.ResponseWriter, r *http.Request) {
	if s.deps.Uploader == nil {
		writeJSON(w, http.StatusOK, uploader.Status{})
		return
	}
	writeJSON(w, http.StatusOK, s.deps.Uploader.Status(r.Context()))
}
