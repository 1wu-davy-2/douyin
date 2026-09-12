package api

// Creators/works/collections endpoints (docs/api.md "博主与作品 Creators /
// Works" + "合集 Collections"). Replaces the stage-1 placeholder.
//
//	POST   /api/creators              -> 202 {creator_id, scan_id}
//	GET    /api/creators              -> [creator]
//	GET    /api/creators/{id}         -> creator + last_scan
//	DELETE /api/creators/{id}
//	POST   /api/creators/{id}/rescan  -> 202 {scan_id}
//	GET    /api/creators/{id}/collections
//	GET    /api/creators/{id}/works   -> paged works
//	GET    /api/collections/{id}/works
//	GET    /api/works/{id}            -> detail (mix_info/assets/last_job)
//	POST   /api/works/batch-ids       -> {ids:[...]}
//	GET    /api/works/{id}/qualities  -> live quality probe via provider
//
// dl_status is never stored: it is derived from the work's latest
// download_jobs row (id DESC LIMIT 1) in the works listing SQL.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"douyin/backend/internal/db"
	"douyin/backend/internal/scanner"
)

func (s *Server) registerCreatorRoutes(mux *http.ServeMux) {
	register(mux, []endpoint{
		{http.MethodPost, "/api/creators", s.handleCreateCreator},
		{http.MethodGet, "/api/creators", s.handleListCreators},
		{http.MethodGet, "/api/creators/{id}", s.handleGetCreator},
		{http.MethodDelete, "/api/creators/{id}", s.handleDeleteCreator},
		{http.MethodPost, "/api/creators/{id}/rescan", s.handleRescanCreator},
		{http.MethodGet, "/api/creators/{id}/collections", s.handleCreatorCollections},
		{http.MethodGet, "/api/creators/{id}/works", s.handleCreatorWorks},
		{http.MethodGet, "/api/collections/{id}/works", s.handleCollectionWorks},
		{http.MethodGet, "/api/works/{id}", s.handleGetWork},
		{http.MethodPost, "/api/works/batch-ids", s.handleBatchWorkIDs},
		{http.MethodGet, "/api/works/{id}/qualities", s.handleWorkQualities},
	})
}

// secUIDPattern extracts a Douyin sec_uid (always "MS4w"-prefixed) from a
// profile URL like https://www.douyin.com/user/MS4wLjABAAAA...?x=y or from a
// bare sec_uid.
var secUIDPattern = regexp.MustCompile(`MS4w[A-Za-z0-9_-]{16,}`)

// extractSecUID returns the sec_uid contained in the input, or "".
func extractSecUID(input string) string {
	return secUIDPattern.FindString(strings.TrimSpace(input))
}

// ------------------------------------------------------------------ creators --

type creatorView struct {
	ID                int64         `json:"id"`
	SecUID            string        `json:"sec_uid"`
	Nickname          string        `json:"nickname"`
	AvatarURL         string        `json:"avatar_url"`
	ProfileURL        string        `json:"profile_url"`
	ReportedWorkCount int64         `json:"reported_work_count"`
	WorksCount        int64         `json:"works_count"`
	DownloadedCount   int64         `json:"downloaded_count"`
	DownloadBytes     int64         `json:"download_bytes"`
	CreatedAt         string        `json:"created_at"`
	LastScan          *lastScanView `json:"last_scan,omitempty"`
}

type lastScanView struct {
	ID           int64   `json:"id"`
	Status       string  `json:"status"`
	Pages        int     `json:"pages"`
	NewCount     int     `json:"new_count"`
	UpdatedCount int     `json:"updated_count"`
	EmptyPages   int     `json:"empty_pages"`
	Completeness int     `json:"completeness"`
	StartedAt    string  `json:"started_at"`
	FinishedAt   *string `json:"finished_at"`
	LastError    *string `json:"last_error"`
}

const creatorListQuery = `
	SELECT c.id, c.sec_uid, c.nickname, c.avatar_url, c.profile_url, c.reported_work_count, c.created_at,
	       (SELECT COUNT(*) FROM works w WHERE w.creator_id = c.id AND w.deleted_at IS NULL) AS works_count,
	       (SELECT COUNT(DISTINCT dj.work_id) FROM download_jobs dj
	        JOIN works w2 ON w2.id = dj.work_id
	        WHERE w2.creator_id = c.id AND dj.status = 'succeeded' AND w2.deleted_at IS NULL) AS downloaded_count,
	       COALESCE((SELECT SUM(a.size_bytes) FROM assets a
	        JOIN works w3 ON w3.id = a.work_id
	        WHERE w3.creator_id = c.id AND a.kind IN ('video', 'image')), 0) AS download_bytes
	FROM creators c`

func scanCreatorView(rows *sql.Rows) (creatorView, error) {
	var v creatorView
	err := rows.Scan(&v.ID, &v.SecUID, &v.Nickname, &v.AvatarURL, &v.ProfileURL,
		&v.ReportedWorkCount, &v.CreatedAt, &v.WorksCount, &v.DownloadedCount, &v.DownloadBytes)
	return v, err
}

// handleCreateCreator POST /api/creators {profile_url} -> 202 {creator_id,
// scan_id}. The scan runs asynchronously; adding an existing creator returns
// the known row and triggers a new (incremental) scan.
func (s *Server) handleCreateCreator(w http.ResponseWriter, r *http.Request) {
	if s.deps.Scanner == nil {
		writeInternalError(w, errors.New("scanner not wired"))
		return
	}
	var body struct {
		ProfileURL string `json:"profile_url"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	secUID := extractSecUID(body.ProfileURL)
	if secUID == "" {
		writeError(w, http.StatusBadRequest, "cannot parse profile_url: no sec_uid found")
		return
	}

	ctx := r.Context()
	var creatorID int64
	var isNew bool
	err := s.deps.DB.QueryRowContext(ctx,
		`SELECT id FROM creators WHERE sec_uid = ?`, secUID).Scan(&creatorID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		isNew = true
		profileURL := "https://www.douyin.com/user/" + secUID
		res, ierr := s.deps.DB.ExecContext(ctx,
			`INSERT INTO creators (sec_uid, nickname, avatar_url, profile_url, created_at)
			 VALUES (?, '', '', ?, ?)`,
			secUID, profileURL, nowRFC3339())
		if ierr != nil {
			writeInternalError(w, ierr)
			return
		}
		if creatorID, ierr = res.LastInsertId(); ierr != nil {
			writeInternalError(w, ierr)
			return
		}
	case err != nil:
		writeInternalError(w, err)
		return
	}

	// New creators get a full first import; re-adds are incremental.
	scanID, err := s.deps.Scanner.Scan(ctx, creatorID, isNew, scanner.TriggerManual)
	if err != nil {
		if errors.Is(err, scanner.ErrCreatorNotFound) {
			writeError(w, http.StatusNotFound, "creator not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"creator_id": creatorID, "scan_id": scanID})
}

// handleListCreators GET /api/creators -> [creator] by created_at desc.
func (s *Server) handleListCreators(w http.ResponseWriter, r *http.Request) {
	rows, err := s.deps.DB.QueryContext(r.Context(),
		creatorListQuery+` ORDER BY c.created_at DESC, c.id DESC`)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	defer rows.Close()
	out := make([]creatorView, 0)
	for rows.Next() {
		v, err := scanCreatorView(rows)
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

// handleGetCreator GET /api/creators/{id} -> creator + last_scan.
func (s *Server) handleGetCreator(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var v creatorView
	row := s.deps.DB.QueryRowContext(r.Context(), creatorListQuery+` WHERE c.id = ?`, id)
	if err := scanCreatorView2(row, &v); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "creator not found")
			return
		}
		writeInternalError(w, err)
		return
	}

	var ls lastScanView
	var finished, lastErr sql.NullString
	err := s.deps.DB.QueryRowContext(r.Context(), `
		SELECT id, status, pages, new_count, updated_count, empty_pages, completeness,
		       started_at, finished_at, last_error
		FROM scan_runs WHERE creator_id = ? ORDER BY id DESC LIMIT 1`, id).
		Scan(&ls.ID, &ls.Status, &ls.Pages, &ls.NewCount, &ls.UpdatedCount, &ls.EmptyPages,
			&ls.Completeness, &ls.StartedAt, &finished, &lastErr)
	switch {
	case err == nil:
		if finished.Valid {
			ls.FinishedAt = &finished.String
		}
		if lastErr.Valid {
			ls.LastError = &lastErr.String
		}
		v.LastScan = &ls
	case errors.Is(err, sql.ErrNoRows):
		// no scan yet: omit the field
	default:
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func scanCreatorView2(row *sql.Row, v *creatorView) error {
	return row.Scan(&v.ID, &v.SecUID, &v.Nickname, &v.AvatarURL, &v.ProfileURL,
		&v.ReportedWorkCount, &v.CreatedAt, &v.WorksCount, &v.DownloadedCount, &v.DownloadBytes)
}

// handleDeleteCreator DELETE /api/creators/{id} — removes the creator and its
// works/collections/jobs/subscriptions (downloaded files stay). Refused while
// a scan of this creator is in flight.
func (s *Server) handleDeleteCreator(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if s.deps.Scanner != nil && s.deps.Scanner.IsScanning(id) {
		writeError(w, http.StatusConflict, "creator is being scanned, try again later")
		return
	}
	ctx := r.Context()
	err := db.WithTx(ctx, s.deps.DB, func(tx *sql.Tx) error {
		var exists bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM creators WHERE id = ?)`, id).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return errNotFound
		}
		stmts := []string{
			`DELETE FROM assets WHERE work_id IN (SELECT id FROM works WHERE creator_id = ?)`,
			`DELETE FROM download_jobs WHERE creator_id = ?`,
			`DELETE FROM works WHERE creator_id = ?`,
			`DELETE FROM collections WHERE creator_id = ?`,
			`DELETE FROM scan_runs WHERE creator_id = ?`,
			`DELETE FROM subscriptions WHERE creator_id = ?`,
			`DELETE FROM creators WHERE id = ?`,
		}
		for _, q := range stmts {
			if _, err := tx.ExecContext(ctx, q, id); err != nil {
				return err
			}
		}
		return nil
	})
	switch {
	case errors.Is(err, errNotFound):
		writeError(w, http.StatusNotFound, "creator not found")
	case err != nil:
		writeInternalError(w, err)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// handleRescanCreator POST /api/creators/{id}/rescan {full?:bool} -> 202
// {scan_id}; full defaults to incremental.
func (s *Server) handleRescanCreator(w http.ResponseWriter, r *http.Request) {
	if s.deps.Scanner == nil {
		writeInternalError(w, errors.New("scanner not wired"))
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var exists bool
	if err := s.deps.DB.QueryRowContext(r.Context(),
		`SELECT EXISTS(SELECT 1 FROM creators WHERE id = ?)`, id).Scan(&exists); err != nil {
		writeInternalError(w, err)
		return
	}
	if !exists {
		writeError(w, http.StatusNotFound, "creator not found")
		return
	}

	// Body is optional; an empty request body means full=false.
	full := false
	var body struct {
		Full *bool `json:"full"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if !decodeJSON(w, r, &body) {
			return
		}
		if body.Full != nil {
			full = *body.Full
		}
	}

	scanID, err := s.deps.Scanner.Scan(r.Context(), id, full, scanner.TriggerManual)
	if err != nil {
		if errors.Is(err, scanner.ErrCreatorNotFound) {
			writeError(w, http.StatusNotFound, "creator not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"scan_id": scanID})
}

// --------------------------------------------------------------- collections --

type collectionView struct {
	ID              int64  `json:"id"`
	MixID           string `json:"mix_id"`
	Name            string `json:"name"`
	CoverURL        string `json:"cover_url"`
	WorksCount      int64  `json:"works_count"`
	DownloadedCount int64  `json:"downloaded_count"`
}

// handleCreatorCollections GET /api/creators/{id}/collections.
func (s *Server) handleCreatorCollections(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.creatorExists(r.Context(), id); err != nil {
		writeCreatorExistsError(w, err)
		return
	}
	rows, err := s.deps.DB.QueryContext(r.Context(), `
		SELECT c.id, c.mix_id, c.name, c.cover_url,
		       (SELECT COUNT(*) FROM works w WHERE w.collection_id = c.id AND w.deleted_at IS NULL) AS works_count,
		       (SELECT COUNT(DISTINCT dj.work_id) FROM download_jobs dj
		        JOIN works w2 ON w2.id = dj.work_id
		        WHERE w2.collection_id = c.id AND dj.status = 'succeeded' AND w2.deleted_at IS NULL) AS downloaded_count
		FROM collections c WHERE c.creator_id = ? ORDER BY c.id`, id)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	defer rows.Close()
	out := make([]collectionView, 0)
	for rows.Next() {
		var v collectionView
		if err := rows.Scan(&v.ID, &v.MixID, &v.Name, &v.CoverURL, &v.WorksCount, &v.DownloadedCount); err != nil {
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

// --------------------------------------------------------------------- works --

type workItem struct {
	ID                int64   `json:"id"`
	ItemID            string  `json:"item_id"`
	Title             string  `json:"title"`
	CoverURL          string  `json:"cover_url"`
	Duration          int     `json:"duration"`
	PublishedAt       *string `json:"published_at"`
	CollectionID      *int64  `json:"collection_id"`
	CollectionName    *string `json:"collection_name"`
	Type              string  `json:"type"`
	ImageCount        int64   `json:"image_count"`
	DlStatus          string  `json:"dl_status"`
	DownloadedQuality *string `json:"downloaded_quality"`
	CreatedAt         string  `json:"created_at"`
}

type workPage struct {
	Items    []workItem `json:"items"`
	Total    int64      `json:"total"`
	Page     int        `json:"page"`
	PageSize int        `json:"page_size"`
}

// workFilter carries the shared works-list filter (creator/collection + q +
// type + dl). It is used by the works lists, their COUNT query and
// batch-ids, so "select all matching" always agrees with the list.
type workFilter struct {
	creatorID    int64
	collectionID *int64
	q            string
	workType     string // video | image | live; any other value is ignored
	dl           string // none | queued | downloading | succeeded | failed; ignored otherwise
}

// validDlFilters are the contract-accepted dl= values (canceled is not
// offered as a filter and is therefore ignored like any unknown value).
var validDlFilters = map[string]bool{
	"none": true, "queued": true, "downloading": true, "succeeded": true, "failed": true,
}

func (f workFilter) where() (string, []any) {
	where := ` WHERE w.deleted_at IS NULL`
	args := make([]any, 0, 6)
	if f.creatorID > 0 {
		where += ` AND w.creator_id = ?`
		args = append(args, f.creatorID)
	}
	if f.collectionID != nil {
		where += ` AND w.collection_id = ?`
		args = append(args, *f.collectionID)
	}
	if f.q != "" {
		where += ` AND (w.title LIKE ? ESCAPE '\' OR w.item_id LIKE ? ESCAPE '\')`
		escaped := escapeLike(f.q)
		args = append(args, "%"+escaped+"%", "%"+escaped+"%")
	}
	switch f.workType {
	case "video":
		where += ` AND w.type = 'video'`
	case "image":
		where += ` AND w.type = 'image'`
	case "live":
		// Live gallery: an image work carrying live video clips
		// (assets.kind='video', quality LIKE 'live%').
		where += ` AND w.type = 'image' AND EXISTS(SELECT 1 FROM assets la` +
			` WHERE la.work_id = w.id AND la.kind = 'video' AND la.quality LIKE 'live%')`
	}
	if validDlFilters[f.dl] {
		// Same CASE derivation as the dl_status badge in the list response
		// (queued includes paused_q; no job row -> none).
		where += ` AND ` + dlStatusExpr + ` = ?`
		args = append(args, f.dl)
	}
	return where, args
}

// joins returns the extra FROM joins the filter needs. dl filtering derives
// the status from the work's latest job (alias lj — the same join the works
// list uses for display); the other filters touch only works/assets.
func (f workFilter) joins() string {
	if validDlFilters[f.dl] {
		return latestJobJoin
	}
	return ""
}

func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// dlStatusExpr maps the latest job per work to the contract's dl_status
// enumeration (paused_q folds into queued; no job -> none).
const dlStatusExpr = `CASE COALESCE(lj.status, '')
	WHEN 'queued' THEN 'queued'
	WHEN 'paused_q' THEN 'queued'
	WHEN 'downloading' THEN 'downloading'
	WHEN 'succeeded' THEN 'succeeded'
	WHEN 'failed' THEN 'failed'
	WHEN 'canceled' THEN 'canceled'
	ELSE 'none' END`

const worksListQuery = `
	SELECT w.id, w.item_id, w.title, w.cover_url, w.duration, w.published_at,
	       w.collection_id, c.name, w.created_at,
	       w.type,
	       (SELECT COUNT(*) FROM assets ia WHERE ia.work_id = w.id AND ia.kind = 'image'),
	       ` + dlStatusExpr + `,
	       CASE WHEN lj.status = 'succeeded' THEN lj.quality END
	FROM works w
	LEFT JOIN collections c ON c.id = w.collection_id`

// latestJobJoin exposes the work's newest download job as lj (id DESC LIMIT
// 1). dlStatusExpr derives dl_status from lj.status, so every query that
// shows or filters on dl_status attaches this identical join.
const latestJobJoin = `
	LEFT JOIN download_jobs lj ON lj.id = (
		SELECT j.id FROM download_jobs j WHERE j.work_id = w.id ORDER BY j.id DESC LIMIT 1)`

// sortClause validates the sort parameter and returns its ORDER BY SQL.
func sortClause(sort string) (string, bool) {
	switch sort {
	case "", "published_at_desc":
		return ` ORDER BY w.published_at DESC, w.id DESC`, true
	case "published_at_asc":
		return ` ORDER BY w.published_at ASC, w.id ASC`, true
	case "duration_desc":
		return ` ORDER BY w.duration DESC, w.id DESC`, true
	default:
		return "", false
	}
}

// parsePageParams validates ?page= & ?page_size= per the contract.
func parsePageParams(r *http.Request) (page, pageSize int, ok bool) {
	page, pageSize = 1, 20
	if v := strings.TrimSpace(r.URL.Query().Get("page")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return 0, 0, false
		}
		page = n
	}
	if v := strings.TrimSpace(r.URL.Query().Get("page_size")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return 0, 0, false
		}
		switch n {
		case 20, 50, 100:
			pageSize = n
		default:
			return 0, 0, false
		}
	}
	return page, pageSize, true
}

// serveWorkList renders the shared paged works response.
func (s *Server) serveWorkList(w http.ResponseWriter, r *http.Request, f workFilter) {
	page, pageSize, ok := parsePageParams(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid page/page_size (page>=1, page_size in 20|50|100)")
		return
	}
	order, ok := sortClause(strings.TrimSpace(r.URL.Query().Get("sort")))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid sort (published_at_desc|published_at_asc|duration_desc)")
		return
	}
	// type/dl filters: unknown values are silently ignored per the contract.
	f.workType = strings.TrimSpace(r.URL.Query().Get("type"))
	f.dl = strings.TrimSpace(r.URL.Query().Get("dl"))

	where, args := f.where()

	var total int64
	if err := s.deps.DB.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM works w`+f.joins()+where, args...).Scan(&total); err != nil {
		writeInternalError(w, err)
		return
	}

	// The list always joins the latest job (dl_status badge); the filter join
	// is the identical LEFT JOIN, so no duplicate alias arises.
	q := worksListQuery + latestJobJoin + where + order + ` LIMIT ? OFFSET ?`
	args = append(args, pageSize, (page-1)*pageSize)
	rows, err := s.deps.DB.QueryContext(r.Context(), q, args...)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	defer rows.Close()

	items := make([]workItem, 0)
	for rows.Next() {
		var it workItem
		var collectionID, collectionName, downloadedQuality sql.NullString
		var published sql.NullString
		if err := rows.Scan(&it.ID, &it.ItemID, &it.Title, &it.CoverURL, &it.Duration, &published,
			&collectionID, &collectionName, &it.CreatedAt, &it.Type, &it.ImageCount,
			&it.DlStatus, &downloadedQuality); err != nil {
			writeInternalError(w, err)
			return
		}
		if published.Valid {
			it.PublishedAt = &published.String
		}
		if collectionID.Valid {
			if cid, err := strconv.ParseInt(collectionID.String, 10, 64); err == nil {
				it.CollectionID = &cid
			}
		}
		if collectionName.Valid {
			it.CollectionName = &collectionName.String
		}
		if downloadedQuality.Valid {
			it.DownloadedQuality = &downloadedQuality.String
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, workPage{Items: items, Total: total, Page: page, PageSize: pageSize})
}

// handleCreatorWorks GET /api/creators/{id}/works.
func (s *Server) handleCreatorWorks(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.creatorExists(r.Context(), id); err != nil {
		writeCreatorExistsError(w, err)
		return
	}
	f := workFilter{creatorID: id}
	if raw := strings.TrimSpace(r.URL.Query().Get("collection_id")); raw != "" {
		cid, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid collection_id")
			return
		}
		f.collectionID = &cid
	}
	f.q = strings.TrimSpace(r.URL.Query().Get("q"))
	s.serveWorkList(w, r, f)
}

// handleCollectionWorks GET /api/collections/{id}/works (same parameters as
// the creator works list).
func (s *Server) handleCollectionWorks(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var creatorID int64
	err := s.deps.DB.QueryRowContext(r.Context(),
		`SELECT creator_id FROM collections WHERE id = ?`, id).Scan(&creatorID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "collection not found")
		return
	}
	if err != nil {
		writeInternalError(w, err)
		return
	}
	f := workFilter{creatorID: creatorID, collectionID: &id}
	f.q = strings.TrimSpace(r.URL.Query().Get("q"))
	s.serveWorkList(w, r, f)
}

// handleGetWork GET /api/works/{id} — detail with mix_info, downloaded assets
// and the latest job summary.
func (s *Server) handleGetWork(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var detail struct {
		workItem
		CreatorID int64        `json:"creator_id"`
		DeletedAt *string      `json:"deleted_at"`
		MixInfo   *mixInfoView `json:"mix_info"`
		Assets    []assetView  `json:"assets"`
		LastJob   *jobSummary  `json:"last_job"`
	}
	var collectionID, collectionName, mixID sql.NullString
	var published, deleted sql.NullString
	var imageCount int64
	err := s.deps.DB.QueryRowContext(r.Context(), `
		SELECT w.id, w.item_id, w.creator_id, w.title, w.cover_url, w.duration, w.published_at,
		       w.collection_id, c.name, c.mix_id, w.created_at, w.deleted_at, w.type,
		       (SELECT COUNT(*) FROM assets ia WHERE ia.work_id = w.id AND ia.kind = 'image')
		FROM works w
		LEFT JOIN collections c ON c.id = w.collection_id
		WHERE w.id = ?`, id).
		Scan(&detail.ID, &detail.ItemID, &detail.CreatorID, &detail.Title, &detail.CoverURL,
			&detail.Duration, &published, &collectionID, &collectionName, &mixID,
			&detail.CreatedAt, &deleted, &detail.Type, &imageCount)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "work not found")
		return
	}
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if published.Valid {
		detail.PublishedAt = &published.String
	}
	if deleted.Valid {
		detail.DeletedAt = &deleted.String
	}
	detail.ImageCount = imageCount
	if collectionID.Valid {
		if cid, perr := strconv.ParseInt(collectionID.String, 10, 64); perr == nil {
			detail.CollectionID = &cid
			detail.MixInfo = &mixInfoView{ID: cid, MixID: mixID.String, Name: collectionName.String}
		}
	}

	// Assets in the contract order: video (newest first) -> image (quality
	// sequence ascending) -> cover -> metadata.
	rows, err := s.deps.DB.QueryContext(r.Context(), `
		SELECT id, kind, path, size_bytes, quality, created_at
		FROM assets WHERE work_id = ?
		`+assetOrder, id)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	defer rows.Close()
	detail.Assets = make([]assetView, 0)
	for rows.Next() {
		var a assetView
		var quality sql.NullString
		if err := rows.Scan(&a.ID, &a.Kind, &a.Path, &a.SizeBytes, &quality, &a.CreatedAt); err != nil {
			writeInternalError(w, err)
			return
		}
		if quality.Valid {
			a.Quality = &quality.String
		}
		detail.Assets = append(detail.Assets, a)
	}
	if err := rows.Err(); err != nil {
		writeInternalError(w, err)
		return
	}
	rows.Close()

	// Latest job summary (same derivation as dl_status).
	var job jobSummary
	jerr := s.deps.DB.QueryRowContext(r.Context(), `
		SELECT id, status, quality, attempts, total_bytes, downloaded_bytes, error,
		       queued_at, started_at, finished_at
		FROM download_jobs WHERE work_id = ? ORDER BY id DESC LIMIT 1`, id).
		Scan(&job.ID, &job.Status, &job.Quality, &job.Attempts, &job.TotalBytes,
			&job.DownloadedBytes, &job.Error, &job.QueuedAt, &job.StartedAt, &job.FinishedAt)
	switch {
	case jerr == nil:
		detail.LastJob = &job
	case errors.Is(jerr, sql.ErrNoRows):
		// no job yet
	default:
		writeInternalError(w, jerr)
		return
	}

	writeJSON(w, http.StatusOK, detail)
}

type mixInfoView struct {
	ID    int64  `json:"id"`
	MixID string `json:"mix_id"`
	Name  string `json:"name"`
}

type assetView struct {
	ID        int64   `json:"id"`
	Kind      string  `json:"kind"`
	Path      string  `json:"path"`
	SizeBytes int64   `json:"size_bytes"`
	Quality   *string `json:"quality"`
	CreatedAt string  `json:"created_at"`
}

type jobSummary struct {
	ID              int64   `json:"id"`
	Status          string  `json:"status"`
	Quality         string  `json:"quality"`
	Attempts        int     `json:"attempts"`
	TotalBytes      int64   `json:"total_bytes"`
	DownloadedBytes int64   `json:"downloaded_bytes"`
	Error           *string `json:"error"`
	QueuedAt        string  `json:"queued_at"`
	StartedAt       *string `json:"started_at"`
	FinishedAt      *string `json:"finished_at"`
}

// handleBatchWorkIDs POST /api/works/batch-ids {creator_id, q?, collection_id?,
// type?, dl?} -> {ids:[...]} for "select all matching". The filters are the
// exact same construction as the works lists (workFilter).
func (s *Server) handleBatchWorkIDs(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CreatorID    int64   `json:"creator_id"`
		Q            *string `json:"q"`
		CollectionID *int64  `json:"collection_id"`
		Type         *string `json:"type"`
		Dl           *string `json:"dl"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.CreatorID <= 0 {
		writeError(w, http.StatusBadRequest, "creator_id is required")
		return
	}
	if err := s.creatorExists(r.Context(), body.CreatorID); err != nil {
		writeCreatorExistsError(w, err)
		return
	}
	f := workFilter{creatorID: body.CreatorID, collectionID: body.CollectionID}
	if body.Q != nil {
		f.q = strings.TrimSpace(*body.Q)
	}
	if body.Type != nil {
		f.workType = strings.TrimSpace(*body.Type)
	}
	if body.Dl != nil {
		f.dl = strings.TrimSpace(*body.Dl)
	}
	where, args := f.where()
	q := `SELECT w.id FROM works w` + f.joins() + where + ` ORDER BY w.published_at DESC, w.id DESC`
	rows, err := s.deps.DB.QueryContext(r.Context(), q, args...)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	defer rows.Close()
	ids := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			writeInternalError(w, err)
			return
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ids": ids})
}

type qualityInfo struct {
	Quality   string `json:"quality"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	Bitrate   int    `json:"bitrate"`
	SizeBytes int64  `json:"size_bytes"`
}

// handleWorkQualities GET /api/works/{id}/qualities — live resolution through
// the provider (mock under DY_MOCK). A provider failure answers 503.
func (s *Server) handleWorkQualities(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var itemID string
	err := s.deps.DB.QueryRowContext(r.Context(),
		`SELECT item_id FROM works WHERE id = ?`, id).Scan(&itemID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "work not found")
		return
	}
	if err != nil {
		writeInternalError(w, err)
		return
	}

	detail, err := s.deps.Resolver.Resolve(r.Context()).WorkDetail(r.Context(), itemID)
	if err != nil {
		// Sidecar offline / upstream failure is a dependency error, not a bug.
		writeError(w, http.StatusServiceUnavailable, "provider unavailable: "+err.Error())
		return
	}

	out := make([]qualityInfo, 0, len(detail.Variants))
	for _, v := range detail.Variants {
		out = append(out, qualityInfo{
			Quality:   v.Quality,
			Width:     v.Width,
			Height:    v.Height,
			Bitrate:   v.Bitrate,
			SizeBytes: v.SizeBytes,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// ------------------------------------------------------------------- helpers --

var errNotFound = errors.New("not found")

func (s *Server) creatorExists(ctx context.Context, id int64) error {
	var exists bool
	if err := s.deps.DB.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM creators WHERE id = ?)`, id).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return errNotFound
	}
	return nil
}

func writeCreatorExistsError(w http.ResponseWriter, err error) {
	if errors.Is(err, errNotFound) {
		writeError(w, http.StatusNotFound, "creator not found")
		return
	}
	writeInternalError(w, err)
}

// pathID parses the {id} path segment; answers 400 on garbage.
func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := r.PathValue("id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid id %q", raw))
		return 0, false
	}
	return id, true
}
