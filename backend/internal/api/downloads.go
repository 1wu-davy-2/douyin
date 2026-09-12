package api

// Download queue endpoints (docs/api.md "下载任务 Downloads"). Replaces the
// stage-1 placeholder.
//
//	POST   /api/downloads                 -> 202 {created, skipped}
//	GET    /api/downloads                  -> cursor-paged job list
//	GET    /api/downloads/summary          -> {queued, downloading, ..., paused, concurrency}
//	POST   /api/downloads/{id}/retry       -> {ok}
//	POST   /api/downloads/{id}/cancel      -> {ok}
//	DELETE /api/downloads/{id}             -> {ok}
//	POST   /api/downloads/batch            -> {affected}
//	POST   /api/downloads/retry-failed     -> {affected}
//	POST   /api/downloads/clear-completed  -> {affected}
//	POST   /api/downloads/queue/pause      -> {ok}
//	POST   /api/downloads/queue/resume     -> {ok}
//
// State mutations delegate to the downloader (which owns the dispatch
// channel, cancel contexts and the pause flag); list/summary query SQLite
// directly with the dl_status semantics shared with creators.go.

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"douyin/backend/internal/downloader"
)

func (s *Server) registerDownloadRoutes(mux *http.ServeMux) {
	register(mux, []endpoint{
		{http.MethodPost, "/api/downloads", s.handleCreateDownloads},
		{http.MethodGet, "/api/downloads", s.handleListDownloads},
		{http.MethodGet, "/api/downloads/summary", s.handleDownloadsSummary},
		{http.MethodPost, "/api/downloads/{id}/retry", s.handleRetryJob},
		{http.MethodPost, "/api/downloads/{id}/cancel", s.handleCancelJob},
		{http.MethodDelete, "/api/downloads/{id}", s.handleDeleteJob},
		{http.MethodPost, "/api/downloads/batch", s.handleBatchDownloads},
		{http.MethodPost, "/api/downloads/retry-failed", s.handleRetryFailed},
		{http.MethodPost, "/api/downloads/clear-completed", s.handleClearCompleted},
		{http.MethodPost, "/api/downloads/queue/pause", s.handleQueuePause},
		{http.MethodPost, "/api/downloads/queue/resume", s.handleQueueResume},
	})
}

// dl must be wired; every download endpoint answers 500 without it.
func (s *Server) dl() *downloader.Downloader {
	if s.deps.Downloader == nil {
		return nil
	}
	return s.deps.Downloader
}

func (s *Server) requireDownloader(w http.ResponseWriter) bool {
	if s.dl() == nil {
		writeInternalError(w, errors.New("downloader not wired"))
		return false
	}
	return true
}

// writeDownloaderError maps downloader sentinel errors to HTTP statuses.
func writeDownloaderError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, downloader.ErrNotFound):
		writeError(w, http.StatusNotFound, "download job not found")
	case errors.Is(err, downloader.ErrConflict):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, downloader.ErrInvalid):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeInternalError(w, err)
	}
}

// handleCreateDownloads POST /api/downloads {work_ids, quality?} -> 202.
func (s *Server) handleCreateDownloads(w http.ResponseWriter, r *http.Request) {
	if !s.requireDownloader(w) {
		return
	}
	var body struct {
		WorkIDs []int64 `json:"work_ids"`
		Quality string  `json:"quality"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if len(body.WorkIDs) == 0 {
		writeError(w, http.StatusBadRequest, "work_ids is required")
		return
	}
	res, err := s.dl().EnqueueDetailed(r.Context(), body.WorkIDs, body.Quality)
	if err != nil {
		writeDownloaderError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, res)
}

// ---------------------------------------------------------------- job view --

type jobView struct {
	ID              int64   `json:"id"`
	WorkID          int64   `json:"work_id"`
	CreatorID       int64   `json:"creator_id"`
	WorkTitle       string  `json:"work_title"`
	CreatorNickname string  `json:"creator_nickname"`
	Status          string  `json:"status"`
	Quality         string  `json:"quality"`
	Attempts        int     `json:"attempts"`
	TotalBytes      int64   `json:"total_bytes"`
	DownloadedBytes int64   `json:"downloaded_bytes"`
	SpeedBps        int64   `json:"speed_bps"`
	Error           *string `json:"error"`
	QueuedAt        string  `json:"queued_at"`
	StartedAt       *string `json:"started_at"`
	FinishedAt      *string `json:"finished_at"`
}

// jobStatuses is the contract's job status enumeration.
var jobStatuses = map[string]bool{
	downloader.StatusQueued:      true,
	downloader.StatusDownloading: true,
	downloader.StatusPausedQ:     true,
	downloader.StatusSucceeded:   true,
	downloader.StatusFailed:      true,
	downloader.StatusCanceled:    true,
}

// handleListDownloads GET /api/downloads?status=&cursor=&limit= — cursor
// pagination, newest job id first.
func (s *Server) handleListDownloads(w http.ResponseWriter, r *http.Request) {
	q := `SELECT j.id, j.work_id, j.creator_id, j.status, j.quality, j.attempts,
	             j.total_bytes, j.downloaded_bytes, j.error, j.queued_at, j.started_at,
	             j.finished_at, w.title, COALESCE(NULLIF(c.nickname, ''), '未知博主')
	      FROM download_jobs j
	      JOIN works w ON w.id = j.work_id
	      LEFT JOIN creators c ON c.id = j.creator_id`

	where := make([]string, 0, 2)
	args := make([]any, 0, 3)
	if raw := strings.TrimSpace(r.URL.Query().Get("status")); raw != "" {
		statuses := strings.Split(raw, ",")
		ph := make([]string, 0, len(statuses))
		for _, st := range statuses {
			st = strings.TrimSpace(st)
			if !jobStatuses[st] {
				writeError(w, http.StatusBadRequest, "invalid status "+st+
					" (queued|downloading|paused_q|succeeded|failed|canceled)")
				return
			}
			ph = append(ph, "?")
			args = append(args, st)
		}
		where = append(where, "j.status IN ("+strings.Join(ph, ",")+")")
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("cursor")); raw != "" {
		cursor, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || cursor < 0 {
			writeError(w, http.StatusBadRequest, "invalid cursor")
			return
		}
		where = append(where, "j.id < ?")
		args = append(args, cursor)
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY j.id DESC"

	limit := 50
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 200 {
			writeError(w, http.StatusBadRequest, "invalid limit (1-200)")
			return
		}
		limit = n
	}
	args = append(args, limit+1) // fetch one extra to compute next_cursor

	rows, err := s.deps.DB.QueryContext(r.Context(), q, args...)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	defer rows.Close()

	items := make([]jobView, 0, limit)
	var next *int64
	for rows.Next() {
		var v jobView
		var errStr, started, finished sql.NullString
		if err := rows.Scan(&v.ID, &v.WorkID, &v.CreatorID, &v.Status, &v.Quality, &v.Attempts,
			&v.TotalBytes, &v.DownloadedBytes, &errStr, &v.QueuedAt, &started,
			&finished, &v.WorkTitle, &v.CreatorNickname); err != nil {
			writeInternalError(w, err)
			return
		}
		if errStr.Valid {
			v.Error = &errStr.String
		}
		if started.Valid {
			v.StartedAt = &started.String
		}
		if finished.Valid {
			v.FinishedAt = &finished.String
		}
		if s.dl() != nil {
			v.SpeedBps = s.dl().SpeedBps(v.ID)
		}
		if len(items) >= limit {
			// The row just scanned is the (limit+1)-th: more pages exist and
			// the cursor is the LAST id of the current page (contract).
			cursor := items[len(items)-1].ID
			next = &cursor
			break
		}
		items = append(items, v)
	}
	if err := rows.Err(); err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
}

// handleDownloadsSummary GET /api/downloads/summary.
func (s *Server) handleDownloadsSummary(w http.ResponseWriter, r *http.Request) {
	if !s.requireDownloader(w) {
		return
	}
	sum, err := s.dl().Summary(r.Context())
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sum)
}

// --------------------------------------------------------------- mutations --

// handleRetryJob POST /api/downloads/{id}/retry {quality?} — requeue one job
// (failed/canceled/succeeded all retryable; optionally switching quality).
func (s *Server) handleRetryJob(w http.ResponseWriter, r *http.Request) {
	if !s.requireDownloader(w) {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Quality *string `json:"quality"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if !decodeJSON(w, r, &body) {
			return
		}
	}
	if err := s.dl().Retry(r.Context(), id, body.Quality); err != nil {
		writeDownloaderError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleCancelJob POST /api/downloads/{id}/cancel — queued rows are finalized
// directly; downloading jobs are aborted through their context.
func (s *Server) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	if !s.requireDownloader(w) {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.dl().Cancel(r.Context(), id); err != nil {
		writeDownloaderError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleDeleteJob DELETE /api/downloads/{id} — refused for downloading jobs.
func (s *Server) handleDeleteJob(w http.ResponseWriter, r *http.Request) {
	if !s.requireDownloader(w) {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.dl().DeleteJob(r.Context(), id); err != nil {
		writeDownloaderError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleBatchDownloads POST /api/downloads/batch {action, ids, quality?}.
func (s *Server) handleBatchDownloads(w http.ResponseWriter, r *http.Request) {
	if !s.requireDownloader(w) {
		return
	}
	var body struct {
		Action  string  `json:"action"`
		IDs     []int64 `json:"ids"`
		Quality *string `json:"quality"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if len(body.IDs) == 0 {
		writeError(w, http.StatusBadRequest, "ids is required")
		return
	}
	affected, err := s.dl().Batch(r.Context(), body.Action, body.IDs, body.Quality)
	if err != nil {
		writeDownloaderError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"affected": affected})
}

// handleRetryFailed POST /api/downloads/retry-failed — requeue all failed
// jobs with attempts < 5.
func (s *Server) handleRetryFailed(w http.ResponseWriter, r *http.Request) {
	if !s.requireDownloader(w) {
		return
	}
	affected, err := s.dl().RetryFailed(r.Context())
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"affected": affected})
}

// handleClearCompleted POST /api/downloads/clear-completed — drop
// succeeded+canceled records.
func (s *Server) handleClearCompleted(w http.ResponseWriter, r *http.Request) {
	if !s.requireDownloader(w) {
		return
	}
	affected, err := s.dl().ClearCompleted(r.Context())
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"affected": affected})
}

// handleQueuePause POST /api/downloads/queue/pause — stop dispatching new
// jobs (running ones finish).
func (s *Server) handleQueuePause(w http.ResponseWriter, r *http.Request) {
	if !s.requireDownloader(w) {
		return
	}
	if err := s.dl().Pause(r.Context()); err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleQueueResume POST /api/downloads/queue/resume {retry_failed?:bool}.
func (s *Server) handleQueueResume(w http.ResponseWriter, r *http.Request) {
	if !s.requireDownloader(w) {
		return
	}
	var body struct {
		RetryFailed bool `json:"retry_failed"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if !decodeJSON(w, r, &body) {
			return
		}
	}
	if err := s.dl().Resume(r.Context(), body.RetryFailed); err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
