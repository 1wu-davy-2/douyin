package api

// Subscription monitor endpoints (docs/api.md "订阅监控 Subscriptions").
// Replaces the stage-1 placeholder.
//
//	GET    /api/subscriptions                  -> [subscription]
//	POST   /api/subscriptions                  -> create (creator | collection)
//	PATCH  /api/subscriptions/{id}             -> partial update
//	DELETE /api/subscriptions/{id}
//	GET    /api/subscriptions/{id}/new-works   -> monitor-period works detail
//
// next_run_at is projected with the same deterministic +-20% interval jitter
// the scheduler applies (scheduler.NextRunAt), so the UI and the tick loop
// agree on the schedule.

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"douyin/backend/internal/scheduler"
)

func (s *Server) registerSubscriptionRoutes(mux *http.ServeMux) {
	register(mux, []endpoint{
		{http.MethodGet, "/api/subscriptions", s.handleListSubscriptions},
		{http.MethodPost, "/api/subscriptions", s.handleCreateSubscription},
		{http.MethodGet, "/api/subscriptions/{id}/new-works", s.handleSubscriptionNewWorks},
		{http.MethodPatch, "/api/subscriptions/{id}", s.handlePatchSubscription},
		{http.MethodDelete, "/api/subscriptions/{id}", s.handleDeleteSubscription},
	})
}

type subscriptionView struct {
	ID              int64   `json:"id"`
	TargetType      string  `json:"target_type"`
	CreatorID       int64   `json:"creator_id"`
	CollectionID    *int64  `json:"collection_id"`
	CreatorNickname string  `json:"creator_nickname"`
	TargetName      string  `json:"target_name"`
	IntervalMinutes int     `json:"interval_minutes"`
	AutoDownload    bool    `json:"auto_download"`
	Quality         string  `json:"quality"`
	LastRunAt       *string `json:"last_run_at"`
	NextRunAt       *string `json:"next_run_at"`
	Enabled         bool    `json:"enabled"`
	NewWorks        int64   `json:"new_works"`
	NewDownloaded   int64   `json:"new_downloaded"`
	CreatedAt       string  `json:"created_at"`
}

// subscriptionListQuery computes the monitor stats in the same statement:
// new_works counts works recorded since the subscription was created
// (works.created_at >= s.created_at) within the target scope (the whole
// creator for creator targets, the single collection otherwise);
// new_downloaded counts those whose dl_status — derived from the latest job
// with the shared dlStatusExpr — is succeeded.
const subscriptionListQuery = `
	SELECT s.id, s.target_type, s.creator_id, s.collection_id,
	       COALESCE(cr.nickname, ''), s.interval_minutes, s.auto_download, s.quality,
	       s.last_run_at, s.enabled, s.created_at,
	       COALESCE(col.name, ''),
	       (SELECT COUNT(*) FROM works w
	        WHERE w.deleted_at IS NULL AND w.created_at >= s.created_at
	          AND w.creator_id = s.creator_id
	          AND (s.collection_id IS NULL OR w.collection_id = s.collection_id)) AS new_works,
	       (SELECT COUNT(*) FROM works w
	        LEFT JOIN download_jobs lj ON lj.id = (
	            SELECT j.id FROM download_jobs j WHERE j.work_id = w.id ORDER BY j.id DESC LIMIT 1)
	        WHERE w.deleted_at IS NULL AND w.created_at >= s.created_at
	          AND w.creator_id = s.creator_id
	          AND (s.collection_id IS NULL OR w.collection_id = s.collection_id)
	          AND ` + dlStatusExpr + ` = 'succeeded') AS new_downloaded
	FROM subscriptions s
	LEFT JOIN creators cr ON cr.id = s.creator_id
	LEFT JOIN collections col ON col.id = s.collection_id`

// scanSubscriptionStats scans the two trailing monitor-stat columns.
func scanSubscriptionStats(scan func(...any) error, v *subscriptionView) error {
	return scan(&v.NewWorks, &v.NewDownloaded)
}

func scanSubscription(rows *sql.Rows) (subscriptionView, error) {
	var v subscriptionView
	var collectionID, lastRun sql.NullString
	var autoDownload, enabled int
	if err := rows.Scan(&v.ID, &v.TargetType, &v.CreatorID, &collectionID,
		&v.CreatorNickname, &v.IntervalMinutes, &autoDownload, &v.Quality,
		&lastRun, &enabled, &v.CreatedAt, &v.TargetName,
		&v.NewWorks, &v.NewDownloaded); err != nil {
		return v, err
	}
	if collectionID.Valid && collectionID.String != "" {
		if cid, err := strconv.ParseInt(collectionID.String, 10, 64); err == nil {
			v.CollectionID = &cid
		}
	}
	v.AutoDownload = autoDownload != 0
	v.Enabled = enabled != 0
	if lastRun.Valid && lastRun.String != "" {
		lr := lastRun.String
		v.LastRunAt = &lr
	}
	v.NextRunAt = scheduler.NextRunAt(v.ID, v.IntervalMinutes, v.LastRunAt, &v.CreatedAt)
	if v.TargetType == "creator" {
		v.TargetName = v.CreatorNickname
	}
	return v, nil
}

// handleListSubscriptions GET /api/subscriptions -> [subscription].
func (s *Server) handleListSubscriptions(w http.ResponseWriter, r *http.Request) {
	rows, err := s.deps.DB.QueryContext(r.Context(),
		subscriptionListQuery+` ORDER BY s.id DESC`)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	defer rows.Close()
	out := make([]subscriptionView, 0)
	for rows.Next() {
		v, err := scanSubscription(rows)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

var validQualities = map[string]bool{"540p": true, "720p": true, "1080p": true}

// validateSubscriptionInput validates the shared create/patch fields and
// normalizes target references against the database.
func (s *Server) validateSubscriptionInput(ctx context.Context, targetType string,
	creatorID int64, collectionID *int64, quality string) error {

	switch targetType {
	case "creator":
		if collectionID != nil && *collectionID > 0 {
			return errors.New("collection_id is only valid for collection targets")
		}
	case "collection":
		if collectionID == nil || *collectionID <= 0 {
			return errors.New("collection_id is required for collection targets")
		}
		var ownerID int64
		err := s.deps.DB.QueryRowContext(ctx,
			`SELECT creator_id FROM collections WHERE id = ?`, *collectionID).Scan(&ownerID)
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("collection not found")
		}
		if err != nil {
			return err
		}
		if ownerID != creatorID {
			return errors.New("collection does not belong to the given creator")
		}
	default:
		return errors.New("target_type must be creator or collection")
	}

	var exists bool
	if err := s.deps.DB.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM creators WHERE id = ?)`, creatorID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return errors.New("creator not found")
	}
	if quality != "" && !validQualities[quality] {
		return errors.New("quality must be 540p, 720p or 1080p")
	}
	return nil
}

// handleCreateSubscription POST /api/subscriptions.
func (s *Server) handleCreateSubscription(w http.ResponseWriter, r *http.Request) {
	var body struct {
		TargetType      string `json:"target_type"`
		CreatorID       int64  `json:"creator_id"`
		CollectionID    *int64 `json:"collection_id"`
		IntervalMinutes int    `json:"interval_minutes"`
		AutoDownload    bool   `json:"auto_download"`
		Quality         string `json:"quality"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	body.TargetType = strings.ToLower(strings.TrimSpace(body.TargetType))
	body.Quality = strings.TrimSpace(body.Quality)
	if body.Quality == "" {
		body.Quality = "1080p"
	}
	if body.IntervalMinutes < 1 {
		writeError(w, http.StatusBadRequest, "interval_minutes must be >= 1")
		return
	}
	if err := s.validateSubscriptionInput(r.Context(), body.TargetType, body.CreatorID,
		body.CollectionID, body.Quality); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	res, err := s.deps.DB.ExecContext(r.Context(), `
		INSERT INTO subscriptions (target_type, creator_id, collection_id, interval_minutes,
		                           auto_download, quality, enabled, created_at)
		VALUES (?, ?, ?, ?, ?, ?, 1, ?)`,
		body.TargetType, body.CreatorID, nullableInt64(body.CollectionID),
		body.IntervalMinutes, boolInt(body.AutoDownload), body.Quality, nowRFC3339())
	if err != nil {
		writeInternalError(w, err)
		return
	}
	id, err := res.LastInsertId()
	if err != nil {
		writeInternalError(w, err)
		return
	}
	v, ok := s.loadSubscription(w, r, id)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// handlePatchSubscription PATCH /api/subscriptions/{id} — partial update of
// any subscription field.
func (s *Server) handlePatchSubscription(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	current, ok := s.loadSubscription(w, r, id)
	if !ok {
		return
	}

	var body struct {
		TargetType      *string `json:"target_type"`
		CreatorID       *int64  `json:"creator_id"`
		CollectionID    *int64  `json:"collection_id"`
		IntervalMinutes *int    `json:"interval_minutes"`
		AutoDownload    *bool   `json:"auto_download"`
		Quality         *string `json:"quality"`
		Enabled         *bool   `json:"enabled"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	targetType := current.TargetType
	if body.TargetType != nil {
		targetType = strings.ToLower(strings.TrimSpace(*body.TargetType))
	}
	creatorID := current.CreatorID
	if body.CreatorID != nil {
		creatorID = *body.CreatorID
	}
	var collectionID *int64
	if body.CollectionID != nil {
		if *body.CollectionID <= 0 {
			collectionID = nil // explicit clear
		} else {
			collectionID = body.CollectionID
		}
	} else {
		collectionID = current.CollectionID
	}
	interval := current.IntervalMinutes
	if body.IntervalMinutes != nil {
		interval = *body.IntervalMinutes
	}
	quality := current.Quality
	if body.Quality != nil {
		quality = strings.TrimSpace(*body.Quality)
	}
	if interval < 1 {
		writeError(w, http.StatusBadRequest, "interval_minutes must be >= 1")
		return
	}
	if err := s.validateSubscriptionInput(r.Context(), targetType, creatorID,
		collectionID, quality); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	enabled := current.Enabled
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	autoDownload := current.AutoDownload
	if body.AutoDownload != nil {
		autoDownload = *body.AutoDownload
	}

	var collArg any
	if collectionID != nil {
		collArg = *collectionID
	}
	if _, err := s.deps.DB.ExecContext(r.Context(), `
		UPDATE subscriptions SET target_type = ?, creator_id = ?, collection_id = ?,
		       interval_minutes = ?, auto_download = ?, quality = ?, enabled = ?
		WHERE id = ?`,
		targetType, creatorID, collArg, interval, boolInt(autoDownload), quality,
		boolInt(enabled), id); err != nil {
		writeInternalError(w, err)
		return
	}
	v, ok := s.loadSubscription(w, r, id)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// subscriptionNewWorks is the GET /api/subscriptions/{id}/new-works response:
// the works recorded during the monitor period (created after the
// subscription), with the same item shape as the works lists.
type subscriptionNewWorks struct {
	Items      []workItem `json:"items"`
	Total      int64      `json:"total"`
	Downloaded int64      `json:"downloaded"`
}

// handleSubscriptionNewWorks GET /api/subscriptions/{id}/new-works — detail
// behind the list's new_works / new_downloaded monitor stats. Scope mirrors
// subscriptionListQuery exactly: works with created_at >= subscription
// created_at, within the target scope (whole creator for creator targets,
// the single collection otherwise), newest published first.
func (s *Server) handleSubscriptionNewWorks(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	sub, ok := s.loadSubscription(w, r, id)
	if !ok {
		return // loadSubscription wrote 404/500 itself
	}

	where := ` WHERE w.deleted_at IS NULL AND w.created_at >= ? AND w.creator_id = ?`
	args := []any{sub.CreatedAt, sub.CreatorID}
	if sub.CollectionID != nil {
		where += ` AND w.collection_id = ?`
		args = append(args, *sub.CollectionID)
	}

	// new_downloaded: the same dlStatusExpr-over-latest-job derivation the
	// list stats use, so the drawer always agrees with the table numbers.
	downloadedWhere := where + ` AND ` + dlStatusExpr + ` = 'succeeded'`
	var total, downloaded int64
	if err := s.deps.DB.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM works w`+where, args...).Scan(&total); err != nil {
		writeInternalError(w, err)
		return
	}
	if err := s.deps.DB.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM works w`+latestJobJoin+downloadedWhere, args...).Scan(&downloaded); err != nil {
		writeInternalError(w, err)
		return
	}

	rows, err := s.deps.DB.QueryContext(r.Context(),
		worksListQuery+latestJobJoin+where+` ORDER BY w.published_at DESC, w.id DESC`, args...)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	defer rows.Close()
	items := make([]workItem, 0, total)
	for rows.Next() {
		it, err := scanWorkItem(rows)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, subscriptionNewWorks{Items: items, Total: total, Downloaded: downloaded})
}

// handleDeleteSubscription DELETE /api/subscriptions/{id}.
func (s *Server) handleDeleteSubscription(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	res, err := s.deps.DB.ExecContext(r.Context(),
		`DELETE FROM subscriptions WHERE id = ?`, id)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeError(w, http.StatusNotFound, "subscription not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// loadSubscription fetches one subscription view; writes the error response
// itself when something goes wrong (ok=false).
func (s *Server) loadSubscription(w http.ResponseWriter, r *http.Request, id int64) (subscriptionView, bool) {
	row := s.deps.DB.QueryRowContext(r.Context(), subscriptionListQuery+` WHERE s.id = ?`, id)
	var v subscriptionView
	var collectionID, lastRun sql.NullString
	var autoDownload, enabled int
	err := row.Scan(&v.ID, &v.TargetType, &v.CreatorID, &collectionID,
		&v.CreatorNickname, &v.IntervalMinutes, &autoDownload, &v.Quality,
		&lastRun, &enabled, &v.CreatedAt, &v.TargetName,
		&v.NewWorks, &v.NewDownloaded)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "subscription not found")
		return v, false
	}
	if err != nil {
		writeInternalError(w, err)
		return v, false
	}
	if collectionID.Valid && collectionID.String != "" {
		if cid, perr := strconv.ParseInt(collectionID.String, 10, 64); perr == nil {
			v.CollectionID = &cid
		}
	}
	v.AutoDownload = autoDownload != 0
	v.Enabled = enabled != 0
	if lastRun.Valid && lastRun.String != "" {
		lr := lastRun.String
		v.LastRunAt = &lr
	}
	v.NextRunAt = scheduler.NextRunAt(v.ID, v.IntervalMinutes, v.LastRunAt, &v.CreatedAt)
	if v.TargetType == "creator" {
		v.TargetName = v.CreatorNickname
	}
	return v, true
}

func nullableInt64(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
