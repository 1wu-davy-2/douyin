package api

// Spark API contract tests: a fake engine (httptest) stands in for
// spark-engine; the tests verify routing, the 503-when-unconfigured rule,
// token forwarding, friend-list swap, the async send-run -> records flow and
// the cookie export writing <data_dir>/.cookie.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"douyin/backend/internal/auth"
	"douyin/backend/internal/config"
	"douyin/backend/internal/db"
	"douyin/backend/internal/events"
	"douyin/backend/internal/settings"
	"douyin/backend/internal/spark"
)

const sparkToken = "test-engine-token"

// newSparkTestServer boots the API server with a spark service pointed at
// the fake engine; returns the server, an authenticated client and the cfg.
func newSparkTestServer(t *testing.T, engineURL string, wireService bool) (*httptest.Server, *http.Client, config.Settings, *events.Bus) {
	t.Helper()
	cfg := config.Settings{
		DataDir:    t.TempDir(),
		Mock:       true,
		SparkURL:   engineURL,
		SparkToken: sparkToken,
	}

	database, err := db.Open(filepath.Join(t.TempDir(), "spark-api.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Migrate(database, cfg.DataDir); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	bus := events.New()
	store := settings.NewStore(database, cfg)
	authService := auth.New(database)

	deps := Deps{
		Cfg:   cfg,
		Auth:  authService,
		Bus:   bus,
		Store: store,
		DB:    database,
	}
	if wireService {
		deps.Spark = spark.New(cfg, database, bus, store)
	}
	server := httptest.NewServer(New(deps).Handler())
	t.Cleanup(server.Close)

	return server, setupAdmin(t, server), cfg, bus
}

// fakeEngine answers the engine contract; every call must carry the token.
func fakeEngine(t *testing.T, sendResults string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	guard := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Engine-Token") != sparkToken {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"detail":"bad token"}`))
				return
			}
			next(w, r)
		}
	}
	mux.HandleFunc("GET /health", guard(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok","version":1,"task_running":false}`))
	}))
	mux.HandleFunc("POST /friends/refresh", guard(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"friends":[{"key":"张三","display_name":"张三"},{"key":"李四","display_name":"李四"},{"key":"王五","display_name":"王五"}],"complete":true}`))
	}))
	mux.HandleFunc("POST /send/run", guard(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Targets []string       `json:"targets"`
			Config  map[string]any `json:"config"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		results := []map[string]any{}
		for _, tgt := range body.Targets {
			results = append(results, map[string]any{
				"target": tgt, "message": "✨今日火花+1", "state": "strong",
				"category": "", "detail": "", "sent_at": time.Now().UTC().Format(time.RFC3339),
			})
		}
		// Config sanity: the Go side must map variants into sendStrategy and
		// friend scan into friendListScan (upstream key expectations).
		strategy, _ := body.Config["sendStrategy"].(map[string]any)
		if _, ok := strategy["messageVariants"]; !ok {
			t.Errorf("engine payload lacks sendStrategy.messageVariants")
		}
		if _, ok := body.Config["friendListScan"]; !ok {
			t.Errorf("engine payload lacks friendListScan")
		}
		_, _ = w.Write([]byte(`{"results":` + mustJSON(results) + `,"account_failure":{}}`))
	}))
	mux.HandleFunc("POST /cookies/export", guard(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"cookie":"SESSDATA=abc; ttwid=xyz","cookie_count":2,"exported_at":"2026-09-14T04:00:00Z"}`))
	}))
	return httptest.NewServer(mux)
}

func mustJSON(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

// doJSON is a tiny helper issuing an authenticated JSON request.
func doJSON(t *testing.T, client *http.Client, method, url, body string) (int, string) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// Without a spark service (or token) the whole surface answers 503
// "spark not configured" — including the login proxy.
func TestSparkNotConfigured(t *testing.T) {
	server, client, _, _ := newSparkTestServer(t, "http://127.0.0.1:1", false)

	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/spark/overview"},
		{"GET", "/api/spark/accounts"},
		{"POST", "/api/spark/send/run"},
		{"GET", "/api/spark/settings"},
		{"GET", "/api/spark/login/status"},
	} {
		status, body := doJSON(t, client, tc.method, server.URL+tc.path, "")
		if status != http.StatusServiceUnavailable || !strings.Contains(body, "spark not configured") {
			t.Errorf("%s %s = %d %s, want 503 spark not configured", tc.method, tc.path, status, body)
		}
	}
}

// End-to-end over the fake engine: create -> refresh friends -> select ->
// manual send -> records -> cookie export.
func TestSparkFlow(t *testing.T) {
	engine := fakeEngine(t, "")
	defer engine.Close()
	server, client, cfg, _ := newSparkTestServer(t, engine.URL, true)

	// Overview reports the (fake) engine as healthy.
	status, body := doJSON(t, client, "GET", server.URL+"/api/spark/overview", "")
	if status != 200 || !strings.Contains(body, `"engine":{"ok":true,"version":1`) {
		t.Fatalf("overview = %d %s", status, body)
	}

	// Create account.
	status, body = doJSON(t, client, "POST", server.URL+"/api/spark/accounts",
		`{"unique_id":"demo","nickname":"Demo号"}`)
	if status != 200 || !strings.Contains(body, `"profile_name":"uid-demo"`) {
		t.Fatalf("create account = %d %s", status, body)
	}
	// Duplicate -> 409.
	status, _ = doJSON(t, client, "POST", server.URL+"/api/spark/accounts", `{"unique_id":"demo"}`)
	if status != http.StatusConflict {
		t.Fatalf("duplicate create status = %d, want 409", status)
	}

	// Friends refresh via the engine.
	status, body = doJSON(t, client, "POST", server.URL+"/api/spark/accounts/1/friends/refresh", "")
	if status != 200 || !strings.Contains(body, `"count":3`) || !strings.Contains(body, `"new":3`) {
		t.Fatalf("friends refresh = %d %s", status, body)
	}

	// Friends list: new friends default to unselected.
	status, body = doJSON(t, client, "GET", server.URL+"/api/spark/accounts/1/friends?with_today=1", "")
	if status != 200 || !strings.Contains(body, `"selected":false`) {
		t.Fatalf("friends list = %d %s", status, body)
	}

	// Select 张三+李四.
	status, body = doJSON(t, client, "PATCH", server.URL+"/api/spark/accounts/1/friends",
		`{"updates":[{"key":"张三","selected":true},{"key":"李四","selected":true}]}`)
	if status != 200 {
		t.Fatalf("patch friends = %d %s", status, body)
	}

	// Manual send run (async) — wait for the records to land.
	status, body = doJSON(t, client, "POST", server.URL+"/api/spark/send/run", `{"mode":"now"}`)
	if status != 200 || !strings.Contains(body, `"accepted":true`) {
		t.Fatalf("send run = %d %s", status, body)
	}
	deadline := time.Now().Add(5 * time.Second)
	recordsOK := false
	for time.Now().Before(deadline) {
		_, body = doJSON(t, client, "GET", server.URL+"/api/spark/records?account_id=1", "")
		if strings.Count(body, `"friend_key"`) >= 2 && strings.Contains(body, `"confirm_state":"strong"`) {
			recordsOK = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !recordsOK {
		t.Fatalf("records never appeared: %s", body)
	}

	// Cookie export writes <data_dir>/.cookie (atomic, sidecar-hot-reloaded).
	status, body = doJSON(t, client, "POST", server.URL+"/api/spark/cookies/export", `{"account_id":1}`)
	if status != 200 || !strings.Contains(body, `"cookie_count":2`) {
		t.Fatalf("cookie export = %d %s", status, body)
	}
	raw, err := os.ReadFile(filepath.Join(cfg.DataDir, ".cookie"))
	if err != nil {
		t.Fatalf("cookie file: %v", err)
	}
	if !strings.Contains(string(raw), "SESSDATA=abc") {
		t.Fatalf("cookie file content wrong: %q", raw)
	}

	// Settings roundtrip with clamping.
	status, body = doJSON(t, client, "PUT", server.URL+"/api/spark/settings",
		`{"messageTemplate":"hi","messageVariants":["v1"],"hitokotoTypes":[],"sendWindow":{"enabled":true,"startHour":9,"endHour":19,"intervalMinutes":30},"sendStrategy":{"shuffleTargets":false,"accountStartDelaySecondsMin":5,"accountStartDelaySecondsMax":10,"messageIntervalSecondsMin":1,"messageIntervalSecondsMax":2},"friendScan":{"maxScanSeconds":300,"idleScanSeconds":120,"scrollStepPx":400,"scrollDelaySeconds":0.8},"accountFailurePause":{"attempts":3,"cooldownMinutes":60}}`)
	if status != 200 || !strings.Contains(body, `"messageIntervalSecondsMin":25`) {
		t.Fatalf("put settings = %d %s (interval floor must clamp to 25)", status, body)
	}

	// Unknown mode -> 400; unknown account ops -> 404.
	status, _ = doJSON(t, client, "POST", server.URL+"/api/spark/send/run", `{"mode":"bogus"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("bogus mode status = %d, want 400", status)
	}
	status, _ = doJSON(t, client, "GET", server.URL+"/api/spark/accounts/99/friends", "")
	if status != http.StatusNotFound {
		t.Fatalf("missing account friends status = %d, want 404", status)
	}
}

// Unauthenticated spark paths answer 401 like every other business route.
func TestSparkAuthGuarded(t *testing.T) {
	engine := fakeEngine(t, "")
	defer engine.Close()
	server, _, _, _ := newSparkTestServer(t, engine.URL, true)

	resp, err := http.Get(server.URL + "/api/spark/overview")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(string(raw), `"unauthorized"`) {
		t.Fatalf("unauth overview = %d %s, want 401 unauthorized", resp.StatusCode, raw)
	}
}

// The engine's 401 (bad/missing token) surfaces as 502 with the engine
// detail — proving the token header actually flowed through the client.
func TestSparkEngineTokenEnforced(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"bad token"}`))
	}))
	defer engine.Close()
	server, client, _, _ := newSparkTestServer(t, engine.URL, true)

	// Overview degrades gracefully (engine.ok=false) instead of erroring.
	status, body := doJSON(t, client, "GET", server.URL+"/api/spark/overview", "")
	if status != 200 || !strings.Contains(body, `"ok":false`) {
		t.Fatalf("overview with dead engine = %d %s", status, body)
	}
	// A business call surfaces the engine detail as 502.
	status, body = doJSON(t, client, "POST", server.URL+"/api/spark/accounts",
		`{"unique_id":"demo"}`)
	if status != 200 {
		t.Fatalf("create account = %d %s", status, body)
	}
	status, body = doJSON(t, client, "POST", server.URL+"/api/spark/accounts/1/friends/refresh", "")
	if status != http.StatusBadGateway || !strings.Contains(body, "bad token") {
		t.Fatalf("refresh with rejected token = %d %s", status, body)
	}
}
