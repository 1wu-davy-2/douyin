package api

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"douyin/backend/internal/auth"
	"douyin/backend/internal/config"
	"douyin/backend/internal/db"
	"douyin/backend/internal/downloader"
	"douyin/backend/internal/events"
	"douyin/backend/internal/provider"
	"douyin/backend/internal/scanner"
	"douyin/backend/internal/settings"
	"douyin/backend/internal/sidecar"
)

func newTestServer(t *testing.T) (*httptest.Server, *events.Bus, *sql.DB) {
	t.Helper()
	cfg := config.Settings{DataDir: t.TempDir(), Mock: true}

	database, err := db.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Migrate(database); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	bus := events.New()
	mgr := sidecar.NewManager(cfg.SidecarPort, "python", true, cfg.SidecarIdleTimeout)
	t.Cleanup(mgr.Stop)
	store := settings.NewStore(database, cfg)
	authService := auth.New(database)
	resolver := provider.NewResolver(cfg, mgr, store)
	scanSvc := scanner.New(context.Background(), resolver, database, bus, store)

	// Downloader (stage 5): BaseURL points at this test server so the mock
	// provider's relative /mockcdn/... URLs resolve against its own fake CDN.
	dl := downloader.New(context.Background(), downloader.Deps{
		DB:      database,
		Bus:     bus,
		Store:   store,
		Source:  resolver,
		DataDir: cfg.DataDir,
	})

	server := httptest.NewServer(New(Deps{
		Cfg:        cfg,
		Auth:       authService,
		Bus:        bus,
		Manager:    mgr,
		Store:      store,
		Resolver:   resolver,
		DB:         database,
		Scanner:    scanSvc,
		Downloader: dl,
	}).Handler())
	t.Cleanup(func() {
		dl.Stop(3 * time.Second)
		server.Close()
	})

	dl.SetBaseURL(server.URL)
	if _, err := dl.RecoverStale(); err != nil {
		t.Fatalf("recover stale: %v", err)
	}
	if err := dl.Start(); err != nil {
		t.Fatalf("start downloader: %v", err)
	}
	return server, bus, database
}

// setupAdmin bootstraps the admin and returns a client carrying the session.
func setupAdmin(t *testing.T, server *httptest.Server) *http.Client {
	t.Helper()
	resp, err := http.Post(server.URL+"/api/auth/setup", "application/json",
		strings.NewReader(`{"username":"admin","password":"test-password"}`))
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("setup status = %d body = %s", resp.StatusCode, raw)
	}
	transport := &cookieTransport{base: http.DefaultTransport}
	for _, c := range resp.Cookies() {
		if c.Name == auth.CookieName {
			transport.cookie = c
		}
	}
	if transport.cookie == nil {
		t.Fatal("setup response carried no session cookie")
	}
	return &http.Client{Transport: transport}
}

type cookieTransport struct {
	base   http.RoundTripper
	cookie *http.Cookie
}

func (t *cookieTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.cookie != nil {
		req = req.Clone(req.Context())
		req.AddCookie(t.cookie)
	}
	return t.base.RoundTrip(req)
}

// Unauthenticated business paths must answer 401 {"detail":"unauthorized"}
// (including routes not yet implemented, e.g. /api/creators), while /api/health
// and /api/auth/* stay public.
func TestAuthGuard(t *testing.T) {
	server, _, _ := newTestServer(t)

	cases := []struct {
		path   string
		status int
	}{
		{"/api/health", http.StatusOK},
		{"/api/auth/status", http.StatusOK},
		{"/api/auth/login", http.StatusMethodNotAllowed}, // registered POST-only, but public
		{"/api/settings", http.StatusUnauthorized},
		{"/api/creators", http.StatusUnauthorized},
		{"/api/creators/1/works", http.StatusUnauthorized},
		{"/api/downloads", http.StatusUnauthorized},
		{"/api/downloads/summary", http.StatusUnauthorized},
		{"/api/subscriptions", http.StatusUnauthorized},
		{"/api/works/9/assets", http.StatusUnauthorized},
		{"/api/events", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		resp, err := http.Get(server.URL + tc.path)
		if err != nil {
			t.Fatalf("GET %s: %v", tc.path, err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.status {
			t.Errorf("GET %s (no cookie): status = %d, want %d (body %s)", tc.path, resp.StatusCode, tc.status, raw)
		}
		if resp.StatusCode == http.StatusUnauthorized && !strings.Contains(string(raw), `"unauthorized"`) {
			t.Errorf("GET %s: body %q lacks contract detail", tc.path, raw)
		}
	}
}

// Health reflects the effective provider (mock, since the test deps set mock).
func TestHealthShape(t *testing.T) {
	server, _, _ := newTestServer(t)
	resp, err := http.Get(server.URL + "/api/health")
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	for _, want := range []string{`"status":"ok"`, `"provider":"mock"`, `"sidecar":"stopped"`, `"real_scan_ready":false`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("health body %s missing %s", raw, want)
		}
	}
}

// SSE: authenticated clients receive contract-framed events.
func TestSSEStream(t *testing.T) {
	server, bus, _ := newTestServer(t)
	client := setupAdmin(t, server)

	// An SSE stream has no EOF, so bound the whole read with a context
	// deadline; io.ReadAll then returns with everything received so far.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/events", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("sse connect: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}

	// Publish a state event and a progress event from "the scanner".
	bus.Publish(events.Event{Type: events.TypeScanDone, Data: events.ScanDone{
		ScanID: 5, CreatorID: 1, Status: "succeeded", Pages: 26, NewCount: 502,
	}})
	bus.Publish(events.Event{Type: events.TypeDownloadProgress, Data: events.DownloadProgress{
		JobID: 99, DownloadedBytes: 10, TotalBytes: 52,
	}})

	raw := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(resp.Body)
		raw <- b
	}()
	var stream []byte
	select {
	case stream = <-raw:
	case <-time.After(4 * time.Second):
		t.Fatal("SSE read did not unblock on context deadline")
	}
	s := string(stream)
	if !strings.Contains(s, "event: scan.done\n") {
		t.Errorf("stream missing scan.done:\n%s", s)
	}
	if !strings.Contains(s, `data: {"scan_id":5,"creator_id":1,"status":"succeeded","pages":26,"new_count":502,"completeness":0,"last_error":null}`) {
		t.Errorf("scan.done payload wrong:\n%s", s)
	}
	if !strings.Contains(s, "event: download.progress\n") {
		t.Errorf("stream missing download.progress:\n%s", s)
	}
}

// Authenticated settings flow: defaults -> patch -> masked read-back.
func TestSettingsFlow(t *testing.T) {
	server, _, _ := newTestServer(t)
	client := setupAdmin(t, server)

	req, _ := http.NewRequest(http.MethodPatch, server.URL+"/api/settings",
		strings.NewReader(`{"cookie":"SESSDATA=abcdefgh-remainder","download_concurrency":6}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch status = %d body = %s", resp.StatusCode, body)
	}

	resp, err = client.Get(server.URL + "/api/settings")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	body, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"cookie":"SESSDATA..."`) {
		t.Errorf("cookie not masked: %s", body)
	}
	if !strings.Contains(string(body), `"download_concurrency":6`) {
		t.Errorf("patch not persisted: %s", body)
	}
	// Invalid values are rejected with 400.
	req, _ = http.NewRequest(http.MethodPatch, server.URL+"/api/settings",
		strings.NewReader(`{"download_concurrency":99}`))
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("invalid patch: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid patch status = %d body = %s", resp.StatusCode, body)
	}
}

// Static serving: unknown non-/api paths fall back to the embedded index.html.
func TestStaticFallback(t *testing.T) {
	server, _, _ := newTestServer(t)
	resp, err := http.Get(server.URL + "/creators/42/works")
	if err != nil {
		t.Fatalf("static: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type = %q", ct)
	}
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "<!doctype html>") {
		t.Fatalf("index.html fallback missing: %q", raw)
	}
}
