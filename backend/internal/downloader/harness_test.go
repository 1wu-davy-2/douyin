package downloader

// Test infrastructure: an in-process fake CDN (httptest) that tracks
// concurrency, a scripted provider whose WorkDetail URLs point at it, and a
// harness wiring a real Downloader over a temp SQLite database.

import (
	"context"
	"errors"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"douyin/backend/internal/config"
	"douyin/backend/internal/db"
	"douyin/backend/internal/events"
	"douyin/backend/internal/provider"
	"douyin/backend/internal/settings"
)

// ------------------------------------------------------------- fake CDN --

type fakeCDN struct {
	srv *httptest.Server

	mu          sync.Mutex
	inflight    int           // total requests being served right now
	maxInflight int           // peak of the counter above
	requests    []string

	chunkDelay time.Duration
	size       int64
	failures   map[string]int // path -> remaining 500s
}

func newFakeCDN(t *testing.T) *fakeCDN {
	c := &fakeCDN{
		size:     256 << 10,
		failures: make(map[string]int),
	}
	c.srv = httptest.NewServer(http.HandlerFunc(c.handle))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *fakeCDN) handle(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	c.mu.Lock()
	c.requests = append(c.requests, path)
	c.inflight++
	if c.inflight > c.maxInflight {
		c.maxInflight = c.inflight
	}
	if c.failures[path] > 0 {
		c.failures[path]--
		c.inflight--
		c.mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.inflight--
		c.mu.Unlock()
	}()

	size := c.size
	if strings.HasSuffix(path, "cover.jpg") {
		size = 1024
	}
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Content-Length", fmt.Sprint(size))
	w.WriteHeader(http.StatusOK)

	buf := make([]byte, 4096)
	var written int64
	for written < size {
		n := int64(len(buf))
		if size-written < n {
			n = size - written
		}
		if _, err := w.Write(buf[:n]); err != nil {
			return
		}
		written += n
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if c.chunkDelay > 0 {
			time.Sleep(c.chunkDelay)
		}
	}
}

func (c *fakeCDN) urlFor(item, name string) string {
	return c.srv.URL + "/cdn/" + item + "/" + name
}

// hitCount counts recorded requests whose path starts with prefix.
func (c *fakeCDN) hitCount(prefix string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, p := range c.requests {
		if strings.HasPrefix(p, prefix) {
			n++
		}
	}
	return n
}

func (c *fakeCDN) peakInflight() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.maxInflight
}

// --------------------------------------------------------- fake provider --

type fakeProvider struct {
	mu        sync.Mutex
	cdnURL    string
	size      int64
	detail    map[string]*provider.WorkDetail // per-item override
	fail      bool                            // WorkDetail always fails
	failItems map[string]bool                 // per-item WorkDetail failure
}

func (f *fakeProvider) Profile(_ context.Context, secUID string) (*provider.Profile, error) {
	return &provider.Profile{SecUID: secUID, Nickname: "Fake", AwemeCount: 0}, nil
}

func (f *fakeProvider) PostsPage(_ context.Context, _, _ string, _ int) (*provider.PostsPage, error) {
	return &provider.PostsPage{}, nil
}

func (f *fakeProvider) WorkDetail(_ context.Context, itemID string) (*provider.WorkDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail || f.failItems[itemID] {
		return nil, errors.New("fake provider is down")
	}
	if d, ok := f.detail[itemID]; ok {
		return d, nil
	}
	variant := func(quality string, size int64) provider.Variant {
		return provider.Variant{
			Quality: quality, Width: 1920, Height: 1080, Bitrate: 1,
			SizeBytes: size, URL: f.cdnURL + "/cdn/" + itemID + "/" + quality + ".mp4",
		}
	}
	return &provider.WorkDetail{
		ItemID:   itemID,
		Title:    "Work " + itemID,
		CoverURL: f.cdnURL + "/cdn/" + itemID + "/cover.jpg",
		Duration: 10,
		Variants: []provider.Variant{
			variant("1080p", f.size),
			variant("720p", f.size/2),
			variant("540p", f.size/4),
		},
	}, nil
}

func (f *fakeProvider) setFailItem(itemID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failItems == nil {
		f.failItems = make(map[string]bool)
	}
	f.failItems[itemID] = true
}

func (f *fakeProvider) clearFailItem(itemID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.failItems, itemID)
}

func (f *fakeProvider) setDetail(itemID string, d *provider.WorkDetail) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.detail == nil {
		f.detail = make(map[string]*provider.WorkDetail)
	}
	f.detail[itemID] = d
}

type fakeSource struct{ p *fakeProvider }

func (s fakeSource) Resolve(context.Context) provider.Provider { return s.p }

// --------------------------------------------------------------- harness --

type harness struct {
	t       *testing.T
	db      *sql.DB
	store   *settings.Store
	bus     *events.Bus
	cdn     *fakeCDN
	prov    *fakeProvider
	dl      *Downloader
	dataDir string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, dataDir: t.TempDir()}

	database, err := db.Open(filepath.Join(h.dataDir, "downloader.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Migrate(database); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	h.db = database

	h.bus = events.New()
	h.store = settings.NewStore(database, config.Settings{DataDir: h.dataDir})
	h.cdn = newFakeCDN(t)
	h.prov = &fakeProvider{cdnURL: h.cdn.srv.URL, size: h.cdn.size}
	h.dl = New(context.Background(), Deps{
		DB:      database,
		Bus:     h.bus,
		Store:   h.store,
		Source:  fakeSource{h.prov},
		DataDir: h.dataDir,
	})
	t.Cleanup(func() {
		h.dl.Stop(3 * time.Second)
		database.Close()
	})
	return h
}

// start runs RecoverStale + Start and fails the test on error.
func (h *harness) start() {
	h.t.Helper()
	if _, err := h.dl.RecoverStale(); err != nil {
		h.t.Fatalf("recover stale: %v", err)
	}
	if err := h.dl.Start(); err != nil {
		h.t.Fatalf("start: %v", err)
	}
}

// setConcurrency persists the download_concurrency setting.
func (h *harness) setConcurrency(n int) {
	h.t.Helper()
	// Apply returns (cookieChanged, err) - only err matters here.
	if _, err := h.store.Apply(context.Background(), settings.Patch{DownloadConcurrency: &n}); err != nil {
		h.t.Fatalf("set concurrency: %v", err)
	}
}

func (h *harness) creator(nickname, secUID string) int64 {
	h.t.Helper()
	res, err := h.db.Exec(
		`INSERT INTO creators (sec_uid, nickname, profile_url, created_at) VALUES (?, ?, ?, ?)`,
		secUID, nickname, "https://www.douyin.com/user/"+secUID, nowRFC3339())
	if err != nil {
		h.t.Fatalf("insert creator: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		h.t.Fatal(err)
	}
	return id
}

func (h *harness) work(creatorID int64, itemID, title string, collectionID any) int64 {
	h.t.Helper()
	res, err := h.db.Exec(`
		INSERT INTO works (creator_id, collection_id, item_id, title, duration, created_at, updated_at)
		VALUES (?, ?, ?, ?, 30, ?, ?)`,
		creatorID, collectionID, itemID, title, nowRFC3339(), nowRFC3339())
	if err != nil {
		h.t.Fatalf("insert work: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		h.t.Fatal(err)
	}
	return id
}

type jobState struct {
	Status   string
	Quality  string
	Attempts int
	Error    *string
	Total    int64
}

func (h *harness) jobState(jobID int64) jobState {
	h.t.Helper()
	var st jobState
	var errStr sql.NullString
	if err := h.db.QueryRow(
		`SELECT status, quality, attempts, error, total_bytes FROM download_jobs WHERE id = ?`, jobID).
		Scan(&st.Status, &st.Quality, &st.Attempts, &errStr, &st.Total); err != nil {
		h.t.Fatalf("load job %d: %v", jobID, err)
	}
	if errStr.Valid {
		st.Error = &errStr.String
	}
	return st
}

// waitFor polls cond until true or the timeout elapses (checked every 20ms).
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", msg)
}

func (h *harness) enqueue(workIDs []int64, quality string) *EnqueueResult {
	h.t.Helper()
	res, err := h.dl.EnqueueDetailed(context.Background(), workIDs, quality)
	if err != nil {
		h.t.Fatalf("enqueue: %v", err)
	}
	return res
}

// partsUnder returns every leftover .part file below the data dir.
func (h *harness) partsUnder() []string {
	h.t.Helper()
	var out []string
	_ = filepath.WalkDir(h.dataDir, func(path string, d os.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(path, ".part") {
			out = append(out, path)
		}
		return nil
	})
	return out
}

// videoAssetQuality lists the quality values of a work's video assets.
func (h *harness) videoAssetQuality(workID int64) []string {
	h.t.Helper()
	rows, err := h.db.Query(
		`SELECT quality FROM assets WHERE work_id = ? AND kind = 'video' ORDER BY quality`, workID)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var q sql.NullString
		if err := rows.Scan(&q); err != nil {
			h.t.Fatal(err)
		}
		if q.Valid {
			out = append(out, q.String)
		}
	}
	return out
}
