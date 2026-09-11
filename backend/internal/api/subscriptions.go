package api

// Subscription monitor endpoints (docs/api.md "订阅监控 Subscriptions").
// Replaces the stage-1 placeholder.
//
//	GET    /api/subscriptions      -> [subscription]
//	POST   /api/subscriptions      -> create (creator | collection)
//	PATCH  /api/subscriptions/{id} -> partial update
//	DELETE /api/subscriptions/{id}
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
	CreatedAt       string  `json:"created_at"`
}

const subscriptionListQuery = `
	SELECT s.id, s.target_type, s.creator_id, s.collection_id,
	       COALESCE(cr.nickname, ''), s.interval_minutes, s.auto_download, s.quality,
	       s.last_run_at, s.enabled, s.created_at,
	       COALESCE(col.name, '')
	FROM subscriptions s
	LEFT JOIN creators cr ON cr.id = s.creator_id
	LEFT JOIN collections col ON col.id = s.collection_id`

func scanSubscription(rows *sql.Rows) (subscriptionView, error) {
	var v subscriptionView
	var collectionID, lastRun sql.NullString
	var autoDownload, enabled int
	if err := rows.Scan(&v.ID, &v.TargetType, &v.CreatorID, &collectionID,
		&v.CreatorNickname, &v.IntervalMinutes, &autoDownload, &v.Quality,
		&lastRun, &enabled, &v.CreatedAt, &v.TargetName); err != nil {
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
		&lastRun, &enabled, &v.CreatedAt, &v.TargetName)
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
