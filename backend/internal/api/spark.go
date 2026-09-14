package api

// Spark (续火花) endpoints — docs/HUOHUA_EXECUTION_PLAN.md §5.
// Every route sits behind the shared auth guard; when DY_SPARK_TOKEN is
// unset the whole surface answers 503 {"detail":"spark not configured"}.
//
//	GET    /api/spark/overview
//	GET    /api/spark/accounts
//	POST   /api/spark/accounts
//	PATCH  /api/spark/accounts/{id}
//	DELETE /api/spark/accounts/{id}
//	POST   /api/spark/accounts/{id}/friends/refresh
//	GET    /api/spark/accounts/{id}/friends
//	PATCH  /api/spark/accounts/{id}/friends
//	POST   /api/spark/send/run                 (async; progress via SSE)
//	GET    /api/spark/records                  (cursor pagination)
//	GET|PUT /api/spark/settings                (send_config object)
//	POST   /api/spark/cookies/export           (engine export -> data/.cookie)
//	GET    /api/spark/engine/health
//	ANY    /api/spark/login/*                  (reverse proxy incl. noVNC WS)

import (
	"errors"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"

	"douyin/backend/internal/spark"
)

func (s *Server) registerSparkRoutes(mux *http.ServeMux) {
	register(mux, []endpoint{
		{http.MethodGet, "/api/spark/overview", s.handleSparkOverview},
		{http.MethodGet, "/api/spark/accounts", s.handleSparkListAccounts},
		{http.MethodPost, "/api/spark/accounts", s.handleSparkCreateAccount},
		{http.MethodPatch, "/api/spark/accounts/{id}", s.handleSparkPatchAccount},
		{http.MethodDelete, "/api/spark/accounts/{id}", s.handleSparkDeleteAccount},
		{http.MethodPost, "/api/spark/accounts/{id}/friends/refresh", s.handleSparkFriendsRefresh},
		{http.MethodGet, "/api/spark/accounts/{id}/friends", s.handleSparkListFriends},
		{http.MethodPatch, "/api/spark/accounts/{id}/friends", s.handleSparkPatchFriends},
		{http.MethodPost, "/api/spark/send/run", s.handleSparkSendRun},
		{http.MethodGet, "/api/spark/records", s.handleSparkRecords},
		{http.MethodGet, "/api/spark/settings", s.handleSparkGetSettings},
		{http.MethodPut, "/api/spark/settings", s.handleSparkPutSettings},
		{http.MethodPost, "/api/spark/cookies/export", s.handleSparkCookiesExport},
		{http.MethodGet, "/api/spark/engine/health", s.handleSparkEngineHealth},
	})
	// Login bridge (QR + noVNC WebSocket) is a pass-through reverse proxy.
	mux.Handle("/api/spark/login/", s.sparkLoginProxy())
}

// sparkSvc returns the service (nil when not wired).
func (s *Server) sparkSvc() *spark.Service { return s.deps.Spark }

// sparkReady answers 503 when the spark surface is disabled. ok=false means
// the response has been written.
func (s *Server) sparkReady(w http.ResponseWriter) bool {
	if s.deps.Spark == nil || !s.deps.Spark.Configured() {
		writeError(w, http.StatusServiceUnavailable, "spark not configured")
		return false
	}
	return true
}

// mapSparkErr converts store/service errors into contract responses.
// Returns handled=true when a response was written.
func (s *Server) mapSparkErr(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, spark.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, spark.ErrDuplicateAccount):
		writeError(w, http.StatusConflict, "account already exists")
	case errors.Is(err, spark.ErrInvalidFriendList):
		writeError(w, http.StatusBadGateway, err.Error())
	case errors.Is(err, spark.ErrSendBusy):
		writeError(w, http.StatusConflict, "send already running")
	case errors.Is(err, spark.ErrNotConfigured):
		writeError(w, http.StatusServiceUnavailable, "spark not configured")
	default:
		var apiErr *spark.APIError
		if errors.As(err, &apiErr) {
			detail := apiErr.Detail
			if detail == "" {
				detail = "engine error"
			}
			writeError(w, http.StatusBadGateway, detail)
			return true
		}
		writeInternalError(w, err)
	}
	return true
}

// handleSparkOverview GET /api/spark/overview.
func (s *Server) handleSparkOverview(w http.ResponseWriter, r *http.Request) {
	if !s.sparkReady(w) {
		return
	}
	writeJSON(w, http.StatusOK, s.deps.Spark.Overview(r.Context()))
}

// handleSparkListAccounts GET /api/spark/accounts.
func (s *Server) handleSparkListAccounts(w http.ResponseWriter, r *http.Request) {
	if !s.sparkReady(w) {
		return
	}
	accounts, err := s.deps.Spark.Store().ListAccounts()
	if !s.mapSparkErr(w, err) {
		if accounts == nil {
			accounts = []spark.Account{}
		}
		writeJSON(w, http.StatusOK, accounts)
	}
}

type sparkCreateAccountReq struct {
	UniqueID    string `json:"unique_id"`
	Username    string `json:"username"`
	Nickname    string `json:"nickname"`
	ProfileName string `json:"profile_name"`
}

// handleSparkCreateAccount POST /api/spark/accounts.
func (s *Server) handleSparkCreateAccount(w http.ResponseWriter, r *http.Request) {
	if !s.sparkReady(w) {
		return
	}
	var req sparkCreateAccountReq
	if !decodeJSON(w, r, &req) {
		return
	}
	req.UniqueID = strings.TrimSpace(req.UniqueID)
	if req.UniqueID == "" {
		writeError(w, http.StatusBadRequest, "unique_id is required")
		return
	}
	profileName := strings.TrimSpace(req.ProfileName)
	if profileName == "" {
		profileName = "uid-" + req.UniqueID
	}
	acct, err := s.deps.Spark.Store().CreateAccount(req.UniqueID, req.Username, req.Nickname, profileName)
	if !s.mapSparkErr(w, err) {
		writeJSON(w, http.StatusOK, acct)
	}
}

// handleSparkPatchAccount PATCH /api/spark/accounts/{id}.
func (s *Server) handleSparkPatchAccount(w http.ResponseWriter, r *http.Request) {
	if !s.sparkReady(w) {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Enabled  *bool   `json:"enabled"`
		Nickname *string `json:"nickname"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	acct, err := s.deps.Spark.Store().UpdateAccount(id, spark.AccountPatch{
		Enabled: body.Enabled, Nickname: body.Nickname,
	})
	if !s.mapSparkErr(w, err) {
		writeJSON(w, http.StatusOK, acct)
	}
}

// handleSparkDeleteAccount DELETE /api/spark/accounts/{id}.
func (s *Server) handleSparkDeleteAccount(w http.ResponseWriter, r *http.Request) {
	if !s.sparkReady(w) {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if !s.mapSparkErr(w, s.deps.Spark.Store().DeleteAccount(id)) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	}
}

// handleSparkFriendsRefresh POST /api/spark/accounts/{id}/friends/refresh.
func (s *Server) handleSparkFriendsRefresh(w http.ResponseWriter, r *http.Request) {
	if !s.sparkReady(w) {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	total, newCount, err := s.deps.Spark.RefreshFriends(r.Context(), id)
	if !s.mapSparkErr(w, err) {
		writeJSON(w, http.StatusOK, map[string]int{"count": total, "new": newCount})
	}
}

// handleSparkListFriends GET /api/spark/accounts/{id}/friends?selected=&with_today=1.
func (s *Server) handleSparkListFriends(w http.ResponseWriter, r *http.Request) {
	if !s.sparkReady(w) {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	// Explicit 404 for a missing account (an empty friend list would be
	// indistinguishable from a fresh one otherwise).
	if _, err := s.deps.Spark.Store().GetAccount(id); err != nil {
		s.mapSparkErr(w, err)
		return
	}
	var selected *bool
	switch r.URL.Query().Get("selected") {
	case "true", "1":
		v := true
		selected = &v
	case "false", "0":
		v := false
		selected = &v
	}
	localDate := ""
	if r.URL.Query().Get("with_today") == "1" {
		localDate = s.deps.Spark.LocalDate(s.deps.Spark.NowLocal())
	}
	friends, err := s.deps.Spark.Store().ListFriends(id, selected, localDate)
	if !s.mapSparkErr(w, err) {
		if friends == nil {
			friends = []spark.FriendToday{}
		}
		writeJSON(w, http.StatusOK, friends)
	}
}

// handleSparkPatchFriends PATCH /api/spark/accounts/{id}/friends.
func (s *Server) handleSparkPatchFriends(w http.ResponseWriter, r *http.Request) {
	if !s.sparkReady(w) {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Updates []spark.FriendUpdate `json:"updates"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if !s.mapSparkErr(w, s.deps.Spark.Store().PatchFriends(id, body.Updates)) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

// handleSparkSendRun POST /api/spark/send/run — async accept, SSE progress.
func (s *Server) handleSparkSendRun(w http.ResponseWriter, r *http.Request) {
	if !s.sparkReady(w) {
		return
	}
	var body struct {
		Mode       string  `json:"mode"`
		AccountIDs []int64 `json:"account_ids"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Mode == "" {
		body.Mode = "now"
	}
	switch body.Mode {
	case "now", "failed", "unsent":
	default:
		writeError(w, http.StatusBadRequest, "mode must be now|failed|unsent")
		return
	}
	if err := s.deps.Spark.TriggerSend(body.Mode, body.AccountIDs); err != nil {
		s.mapSparkErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"accepted": true})
}

// handleSparkRecords GET /api/spark/records?account_id=&cursor=&limit=.
func (s *Server) handleSparkRecords(w http.ResponseWriter, r *http.Request) {
	if !s.sparkReady(w) {
		return
	}
	q := r.URL.Query()
	var accountID *int64
	if v := q.Get("account_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad account_id")
			return
		}
		accountID = &id
	}
	cursor, _ := strconv.ParseInt(q.Get("cursor"), 10, 64)
	limit := 20
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}
	items, next, err := s.deps.Spark.Store().ListSendRecords(accountID, cursor, limit)
	if !s.mapSparkErr(w, err) {
		if items == nil {
			items = []spark.SendRecord{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
	}
}

// handleSparkGetSettings GET /api/spark/settings.
func (s *Server) handleSparkGetSettings(w http.ResponseWriter, r *http.Request) {
	if !s.sparkReady(w) {
		return
	}
	cfg, err := s.deps.Spark.Store().LoadSendConfig()
	if !s.mapSparkErr(w, err) {
		writeJSON(w, http.StatusOK, cfg)
	}
}

// handleSparkPutSettings PUT /api/spark/settings — whole-object replace.
func (s *Server) handleSparkPutSettings(w http.ResponseWriter, r *http.Request) {
	if !s.sparkReady(w) {
		return
	}
	var cfg spark.SendConfig
	if !decodeJSON(w, r, &cfg) {
		return
	}
	cfg = cfg.Normalize()
	if err := s.deps.Spark.Store().SaveSendConfig(cfg); !s.mapSparkErr(w, err) {
		writeJSON(w, http.StatusOK, cfg)
	}
}

// handleSparkCookiesExport POST /api/spark/cookies/export — engine export ->
// <data_dir>/.cookie (the archive sidecar hot-reloads it).
func (s *Server) handleSparkCookiesExport(w http.ResponseWriter, r *http.Request) {
	if !s.sparkReady(w) {
		return
	}
	var body struct {
		AccountID int64 `json:"account_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.AccountID == 0 {
		writeError(w, http.StatusBadRequest, "account_id is required")
		return
	}
	count, err := s.deps.Spark.ExportCookiesToArchive(r.Context(), body.AccountID)
	if !s.mapSparkErr(w, err) {
		writeJSON(w, http.StatusOK, map[string]int{"cookie_count": count})
	}
}

// handleSparkEngineHealth GET /api/spark/engine/health.
func (s *Server) handleSparkEngineHealth(w http.ResponseWriter, r *http.Request) {
	if s.deps.Spark == nil || !s.deps.Spark.Configured() {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "detail": "spark not configured"})
		return
	}
	h, err := s.deps.Spark.Client().Health(r.Context())
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "detail": "engine unreachable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "version": h.Version, "task_running": h.TaskRunning,
	})
}

// sparkLoginProxy reverse-proxies /api/spark/login/* to the engine's
// /login/* (QR image, status polling, noVNC WebSocket). httputil.ReverseProxy
// transparently tunnels Upgrade requests, so the VNC stream works as-is.
func (s *Server) sparkLoginProxy() http.Handler {
	if s.deps.Spark == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeError(w, http.StatusServiceUnavailable, "spark not configured")
		})
	}
	target, err := url.Parse(s.deps.Spark.BaseURL())
	if err != nil || target.Scheme == "" || target.Host == "" {
		log.Printf("[api] spark login proxy: bad engine URL %q", s.deps.Spark.BaseURL())
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeError(w, http.StatusBadGateway, "bad spark engine URL")
		})
	}
	token := s.deps.Spark.Client().Token()
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		log.Printf("[api] spark login proxy: %v", err)
		writeError(w, http.StatusBadGateway, "spark engine unreachable")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.sparkReady(w) {
			return
		}
		// Rewrite /api/spark/login/X -> /login/X and stamp the engine token.
		r.URL.Path = "/login/" + strings.TrimPrefix(r.URL.Path, "/api/spark/login/")
		r.Host = target.Host
		r.Header.Set("X-Engine-Token", token)
		proxy.ServeHTTP(w, r)
	})
}
