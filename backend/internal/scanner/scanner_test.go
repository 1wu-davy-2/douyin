package scanner

// Scanner behaviour tests with scripted fake providers:
//
//   - transient bad page (2 failures then recovery) -> scan continues, backoffs
//     observed, final status succeeded
//   - persistent bad pages -> partial with empty_pages and last_error
//   - repeated cursor with has_more=true -> fallback cursor recovers the
//     missing segment (no data loss)
//   - repeated cursor with no way forward -> terminates (no infinite loop)
//   - incremental scan of fully-known pages -> early stop succeeded
//   - completeness gap -> partial + completeness
//   - soft delete on full succeeded, revival on re-sight
//   - Enqueuer seam + subscription matching, single-flight dedup, recycle

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

// ---------------------------------------------------------------- test fakes --

// fakeSource adapts a provider.Provider into the ProviderSource seam.
type fakeSource struct{ p provider.Provider }

func (f fakeSource) Resolve(context.Context) provider.Provider { return f.p }

// fakeProvider is a scripted provider: pages maps cursors to pages,
// defaultPage (when set) is served for any unknown cursor, and failures counts
// how many initial requests per cursor must fail before the page is served.
type fakeProvider struct {
	mu          sync.Mutex
	profile     *provider.Profile
	pages       map[string]*provider.PostsPage
	defaultPage *provider.PostsPage
	failures    map[string]int
	calls       []string
	postsHook   func(cursor string) // invoked around each PostsPage call
}

func (f *fakeProvider) Profile(_ context.Context, secUID string) (*provider.Profile, error) {
	if f.profile != nil {
		return f.profile, nil
	}
	return &provider.Profile{SecUID: secUID, Nickname: "Fake", AwemeCount: 0}, nil
}

func (f *fakeProvider) PostsPage(_ context.Context, _ string, cursor string, count int) (*provider.PostsPage, error) {
	f.mu.Lock()
	f.calls = append(f.calls, cursor)
	hook := f.postsHook
	failLeft := 0
	if n, ok := f.failures[cursor]; ok {
		failLeft = n
	}
	page, known := f.pages[cursor]
	f.mu.Unlock()

	if hook != nil {
		hook(cursor)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if failLeft > 0 {
		f.failures[cursor] = failLeft - 1
		return nil, fmt.Errorf("fake upstream 502 at cursor %s", cursor)
	}
	if known {
		return page, nil
	}
	if f.defaultPage != nil {
		return f.defaultPage, nil
	}
	return nil, fmt.Errorf("fake upstream: unexpected cursor %q", cursor)
}

func (f *fakeProvider) WorkDetail(_ context.Context, _ string) (*provider.WorkDetail, error) {
	return nil, errors.New("fake provider: WorkDetail not implemented")
}

// fakeWork renders one deterministic work (published times descend with pos,
// duration in milliseconds per the sidecar contract).
func fakeWork(pos int) provider.PostItem {
	ts := time.Unix(1735689600-int64(pos-1)*86400, 0).UTC().Format(time.RFC3339)
	return provider.PostItem{
		ItemID:      fmt.Sprintf("fake_%04d", pos),
		Title:       fmt.Sprintf("Fake #%d", pos),
		CoverURL:    fmt.Sprintf("https://fake/%d.jpg", pos),
		Duration:    (30 + pos%25) * 1000,
		PublishedAt: ts,
	}
}

// offsetPage builds a mock-style page: cursor=offset -> works offset+1 ..
// offset+n, next cursor = offset+n.
func offsetPage(offset, n, total int) *provider.PostsPage {
	items := make([]provider.PostItem, 0, n)
	for i := offset; i < offset+n && i < total; i++ {
		items = append(items, fakeWork(i+1))
	}
	hasMore := offset+n < total
	p := &provider.PostsPage{Items: items, HasMore: hasMore}
	if hasMore {
		next := fmt.Sprintf("%d", offset+n)
		p.NextCursor = &next
	}
	return p
}

// offsetPages builds the standard provider page set for total works in
// 20-per pages keyed by the decimal offset cursor.
func offsetPages(total int) map[string]*provider.PostsPage {
	pages := map[string]*provider.PostsPage{}
	for offset := 0; offset < total; offset += 20 {
		pages[fmt.Sprintf("%d", offset)] = offsetPage(offset, 20, total)
	}
	return pages
}

// withMix tags every every-th work (1-based position parsed from item_id) with
// a collection, mimicking douyin mix_info.
func withMix(pages map[string]*provider.PostsPage, every int, mixID, mixName string) {
	for _, p := range pages {
		for i := range p.Items {
			pos := 0
			fmt.Sscanf(p.Items[i].ItemID, "fake_%d", &pos)
			if every > 0 && pos%every == 0 {
				p.Items[i].MixID = mixID
				p.Items[i].MixName = mixName
			}
		}
	}
}

// ----------------------------------------------------------------- harness --

type harness struct {
	sc   *Scanner
	dbs  *sql.DB
	bus  *events.Bus
	src  *fakeProvider
	mu   sync.Mutex
	wait []time.Duration // recorded sleep durations
}

func newHarness(t *testing.T, prof *fakeProvider) *harness {
	t.Helper()
	handle, err := db.Open(filepath.Join(t.TempDir(), "scan.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Migrate(handle); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { handle.Close() })

	bus := events.New()
	store := settings.NewStore(handle, config.Settings{DataDir: t.TempDir()})
	h := &harness{
		sc:  New(context.Background(), fakeSource{prof}, handle, bus, store),
		dbs: handle,
		bus: bus,
		src: prof,
	}
	h.sc.sleep = func(ctx context.Context, d time.Duration) error {
		h.mu.Lock()
		h.wait = append(h.wait, d)
		h.mu.Unlock()
		return ctx.Err()
	}
	return h
}

func (h *harness) sleeps() []time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]time.Duration(nil), h.wait...)
}

func (h *harness) calls() []string {
	h.src.mu.Lock()
	defer h.src.mu.Unlock()
	return append([]string(nil), h.src.calls...)
}

// collect gathers bus events until stop is called.
func (h *harness) collect() (progress func() []events.ScanProgress, done func() []events.ScanDone, stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := h.bus.Subscribe(ctx)
	var mu sync.Mutex
	var prog []events.ScanProgress
	var dn []events.ScanDone
	go func() {
		for evt := range ch {
			mu.Lock()
			switch d := evt.Data.(type) {
			case events.ScanProgress:
				prog = append(prog, d)
			case events.ScanDone:
				dn = append(dn, d)
			}
			mu.Unlock()
		}
	}()
	return func() []events.ScanProgress { mu.Lock(); defer mu.Unlock(); return prog },
		func() []events.ScanDone { mu.Lock(); defer mu.Unlock(); return dn },
		cancel
}

func insertCreator(t *testing.T, h *harness, secUID string) int64 {
	t.Helper()
	res, err := h.dbs.Exec(
		`INSERT INTO creators (sec_uid, nickname, profile_url, created_at) VALUES (?, ?, ?, ?)`,
		secUID, "Fake博主", "https://www.douyin.com/user/"+secUID, nowRFC3339())
	if err != nil {
		t.Fatalf("insert creator: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("creator id: %v", err)
	}
	return id
}

func liveWorkCount(t *testing.T, h *harness, creatorID int64) int {
	t.Helper()
	var n int
	if err := h.dbs.QueryRow(
		`SELECT COUNT(*) FROM works WHERE creator_id = ? AND deleted_at IS NULL`, creatorID).Scan(&n); err != nil {
		t.Fatalf("count works: %v", err)
	}
	return n
}

func mustRun(t *testing.T, h *harness, creatorID int64, full bool) Result {
	t.Helper()
	res, err := h.sc.ScanSync(context.Background(), creatorID, full, TriggerManual)
	if err != nil {
		t.Fatalf("ScanSync: %v", err)
	}
	return res
}

// countSleeps reports how many recorded sleeps match the given durations.
func countSleeps(h *harness, want ...time.Duration) map[time.Duration]int {
	got := map[time.Duration]int{}
	for _, d := range h.sleeps() {
		for _, w := range want {
			if d == w {
				got[w]++
			}
		}
	}
	return got
}

// ------------------------------------------------------------------- tests --

// Transient bad page: page 5 (cursor 80) fails twice, then recovers. The scan
// must continue, record the 2s/4s backoffs, and finish clean.
func TestScanRecoversFromTransientBadPage(t *testing.T) {
	const total = 100
	prof := &fakeProvider{
		pages:    offsetPages(total),
		failures: map[string]int{"80": 2},
	}
	h := newHarness(t, prof)
	creatorID := insertCreator(t, h, "MS4wLjABAAAAfake_recovery")
	prof.profile = &provider.Profile{SecUID: "x", Nickname: "Fake", AwemeCount: total}

	progress, done, stop := h.collect()
	defer stop()
	res := mustRun(t, h, creatorID, true)

	if res.Status != StatusSucceeded {
		t.Fatalf("status = %s (%v), want succeeded", res.Status, res.LastError)
	}
	if res.NewCount != total || liveWorkCount(t, h, creatorID) != total {
		t.Fatalf("new=%d db=%d, want %d (no data loss)", res.NewCount, liveWorkCount(t, h, creatorID), total)
	}
	if res.Pages != 5 {
		t.Fatalf("pages = %d, want 5", res.Pages)
	}
	// Exact backoff durations (2s, 4s) must appear once each; page delays are
	// jittered and never land exactly on whole seconds.
	backoffs := countSleeps(h, 2*time.Second, 4*time.Second, 8*time.Second)
	if backoffs[2*time.Second] != 1 || backoffs[4*time.Second] != 1 || backoffs[8*time.Second] != 0 {
		t.Fatalf("backoff sleeps = %v (all sleeps %v), want exactly one 2s and one 4s",
			backoffs, h.sleeps())
	}
	if len(progress()) != 5 {
		t.Fatalf("progress events = %d, want 5", len(progress()))
	}
	if len(done()) != 1 || done()[0].Status != StatusSucceeded || done()[0].NewCount != total {
		t.Fatalf("scan.done = %+v", done())
	}
	// Duration conversion: provider carries milliseconds -> stored seconds.
	var dur int
	if err := h.dbs.QueryRow(`SELECT duration FROM works WHERE item_id = 'fake_0001'`).Scan(&dur); err != nil {
		t.Fatal(err)
	}
	if want := 30 + 1%25; dur != want {
		t.Fatalf("duration = %d s, want %d (provider ms / 1000)", dur, want)
	}
}

// Persistent bad pages: everything past cursor 40 fails forever. After
// scan_max_empty_pages (3) consecutive bad pages the scan terminates with
// status=partial, empty_pages>=3 and last_error set; works fetched before the
// failure remain in the database.
func TestScanPartialOnPersistentBadPages(t *testing.T) {
	const total = 100
	prof := &fakeProvider{pages: map[string]*provider.PostsPage{
		"0":  offsetPage(0, 20, total),
		"20": offsetPage(20, 20, total),
		"40": offsetPage(40, 20, total),
	}}
	h := newHarness(t, prof)
	creatorID := insertCreator(t, h, "MS4wLjABAAAAfake_partial")
	prof.profile = &provider.Profile{SecUID: "x", Nickname: "Fake", AwemeCount: total}

	res := mustRun(t, h, creatorID, true)

	if res.Status != StatusPartial {
		t.Fatalf("status = %s (%v), want partial", res.Status, res.LastError)
	}
	if res.LastError == nil || !strings.Contains(*res.LastError, "failed after 4 attempt") {
		t.Fatalf("last_error = %v, want the retries-exhausted upstream failure", res.LastError)
	}
	if res.EmptyPages < 3 {
		t.Fatalf("empty_pages = %d, want >= 3", res.EmptyPages)
	}
	if got := liveWorkCount(t, h, creatorID); got != 60 {
		t.Fatalf("works in db = %d, want 60", got)
	}
}

// The legacy miss-scan root cause: the provider repeats cursor "20" while
// has_more=true. The scanner must synthesize a fallback cursor from the oldest
// seen publish time (ms - 1) and recover the missing segment - no data loss.
func TestScanCursorFallbackRecoversData(t *testing.T) {
	const total = 60
	works := func(from, to int) []provider.PostItem {
		items := make([]provider.PostItem, 0, to-from+1)
		for p := from; p <= to; p++ {
			items = append(items, fakeWork(p))
		}
		return items
	}
	repeat := "20" // the glitch: page 2 repeats its own cursor
	fallbackPage := &provider.PostsPage{Items: works(41, 60), HasMore: false}
	prof := &fakeProvider{
		pages: map[string]*provider.PostsPage{
			"0":  {Items: works(1, 20), HasMore: true, NextCursor: strPtr("20")},
			"20": {Items: works(21, 40), HasMore: true, NextCursor: &repeat},
		},
		defaultPage: fallbackPage, // served for the synthesized fallback cursor
	}
	h := newHarness(t, prof)
	creatorID := insertCreator(t, h, "MS4wLjABAAAAfake_fallback")
	prof.profile = &provider.Profile{SecUID: "x", Nickname: "Fake", AwemeCount: total}

	res := mustRun(t, h, creatorID, true)

	if res.Status != StatusSucceeded {
		t.Fatalf("status = %s (%v), want succeeded", res.Status, res.LastError)
	}
	if got := liveWorkCount(t, h, creatorID); got != total {
		t.Fatalf("works in db = %d, want %d (fallback failed to recover the tail)", got, total)
	}
	if res.NewCount != total || res.Pages != 3 {
		t.Fatalf("new=%d pages=%d, want 60/3", res.NewCount, res.Pages)
	}
	// The third request must have been the synthesized fallback cursor
	// (oldest seen publish time in ms - 1), NOT the repeated "20".
	calls := h.calls()
	if len(calls) != 3 || calls[2] == "20" || calls[2] == "0" {
		t.Fatalf("cursor requests = %v, want [0 20 <fallback>]", calls)
	}
	if ms, ok := publishedAtMillis(fakeWork(40).PublishedAt); !ok || calls[2] != fmt.Sprintf("%d", ms-1) {
		t.Fatalf("fallback cursor = %q, want %d (oldest seen ms - 1)", calls[2], ms-1)
	}
}

// A repeated cursor that can never be resolved (the fallback returns the same
// data forever) must terminate the scan instead of looping forever.
func TestScanTerminatesOnUnresolvableCursor(t *testing.T) {
	repeat := "0"
	samePage := &provider.PostsPage{
		Items:      []provider.PostItem{fakeWork(1), fakeWork(2)},
		HasMore:    true,
		NextCursor: &repeat,
	}
	prof := &fakeProvider{
		pages:       map[string]*provider.PostsPage{"0": samePage},
		defaultPage: samePage, // the fallback cursor yields the same page
	}
	h := newHarness(t, prof)
	creatorID := insertCreator(t, h, "MS4wLjABAAAAfake_stuck")
	prof.profile = &provider.Profile{SecUID: "x", Nickname: "Fake", AwemeCount: 2}

	done := make(chan Result, 1)
	go func() { done <- mustRun(t, h, creatorID, true) }()
	select {
	case res := <-done:
		if res.Status != StatusPartial {
			t.Fatalf("status = %s (%v), want partial", res.Status, res.LastError)
		}
		if res.LastError == nil || !strings.Contains(*res.LastError, "stuck") {
			t.Fatalf("last_error = %v, want the stuck explanation", res.LastError)
		}
		if got := liveWorkCount(t, h, creatorID); got != 2 {
			t.Fatalf("works = %d, want 2", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("scan did not terminate on unresolvable cursor")
	}
}

// Incremental scan of an up-to-date creator: three fully-known pages trip the
// early stop, and reconciliation keeps the run succeeded.
func TestScanIncrementalEarlyStop(t *testing.T) {
	const total = 100
	prof := &fakeProvider{pages: offsetPages(total)}
	h := newHarness(t, prof)
	creatorID := insertCreator(t, h, "MS4wLjABAAAAfake_incremental")
	prof.profile = &provider.Profile{SecUID: "x", Nickname: "Fake", AwemeCount: total}

	full := mustRun(t, h, creatorID, true)
	if full.Status != StatusSucceeded || full.NewCount != total {
		t.Fatalf("full scan = %+v", full)
	}

	// Fresh provider instance, same data: incremental run stops early.
	prof.pages = offsetPages(total)
	prof.calls = nil
	res := mustRun(t, h, creatorID, false)

	if res.Status != StatusSucceeded {
		t.Fatalf("status = %s (%v), want succeeded", res.Status, res.LastError)
	}
	if res.Pages != 3 {
		t.Fatalf("pages = %d, want 3 (incremental_stop_pages)", res.Pages)
	}
	// Early stop after 3 pages: exactly the 60 visited works counted as known.
	if res.NewCount != 0 || res.UpdatedCount != 60 {
		t.Fatalf("new=%d updated=%d, want 0/60", res.NewCount, res.UpdatedCount)
	}
}

// Completeness reconciliation: the profile claims 502 works but the provider
// only serves 100 -> partial with completeness=402.
func TestScanCompletenessGapForcesPartial(t *testing.T) {
	const served = 100
	prof := &fakeProvider{pages: offsetPages(served)}
	h := newHarness(t, prof)
	creatorID := insertCreator(t, h, "MS4wLjABAAAAfake_gap")
	prof.profile = &provider.Profile{SecUID: "x", Nickname: "Fake", AwemeCount: 502}

	res := mustRun(t, h, creatorID, true)

	if res.Status != StatusPartial {
		t.Fatalf("status = %s, want partial", res.Status)
	}
	if res.Completeness != 402 {
		t.Fatalf("completeness = %d, want 402", res.Completeness)
	}
	if res.LastError == nil || !strings.Contains(*res.LastError, "gap") {
		t.Fatalf("last_error = %v, want the gap explanation", res.LastError)
	}
	if got := liveWorkCount(t, h, creatorID); got != served {
		t.Fatalf("works = %d, want %d", got, served)
	}
}

// Soft delete: a successful full scan hides works it did not see; a later full
// scan that sees them again revives them.
func TestScanSoftDeleteAndRevival(t *testing.T) {
	prof := &fakeProvider{pages: offsetPages(100)}
	h := newHarness(t, prof)
	creatorID := insertCreator(t, h, "MS4wLjABAAAAfake_softdel")
	prof.profile = &provider.Profile{SecUID: "x", Nickname: "Fake", AwemeCount: 100}

	if res := mustRun(t, h, creatorID, true); res.Status != StatusSucceeded {
		t.Fatalf("first full scan: %+v", res)
	}

	// Upstream "removed" works 91..100 and reports accordingly.
	prof.pages = offsetPages(90)
	prof.profile.AwemeCount = 90
	if res := mustRun(t, h, creatorID, true); res.Status != StatusSucceeded {
		t.Fatalf("second full scan: %+v", res)
	}
	var deleted int
	if err := h.dbs.QueryRow(
		`SELECT COUNT(*) FROM works WHERE creator_id = ? AND deleted_at IS NOT NULL`, creatorID).Scan(&deleted); err != nil {
		t.Fatal(err)
	}
	if deleted != 10 {
		t.Fatalf("soft-deleted = %d, want 10", deleted)
	}
	if got := liveWorkCount(t, h, creatorID); got != 90 {
		t.Fatalf("live works = %d, want 90", got)
	}

	// Upstream restores them -> revival clears deleted_at.
	prof.pages = offsetPages(100)
	prof.profile.AwemeCount = 100
	if res := mustRun(t, h, creatorID, true); res.Status != StatusSucceeded {
		t.Fatalf("third full scan: %+v", res)
	}
	if err := h.dbs.QueryRow(
		`SELECT COUNT(*) FROM works WHERE creator_id = ? AND deleted_at IS NOT NULL`, creatorID).Scan(&deleted); err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Fatalf("soft-deleted after revival = %d, want 0", deleted)
	}
	if got := liveWorkCount(t, h, creatorID); got != 100 {
		t.Fatalf("live works = %d, want 100", got)
	}
}

// An incremental scan must NEVER soft-delete (works below the stop point were
// legitimately not visited).
func TestScanIncrementalDoesNotSoftDelete(t *testing.T) {
	const total = 100
	prof := &fakeProvider{pages: offsetPages(total)}
	h := newHarness(t, prof)
	creatorID := insertCreator(t, h, "MS4wLjABAAAAfake_incsoft")
	prof.profile = &provider.Profile{SecUID: "x", Nickname: "Fake", AwemeCount: total}
	if res := mustRun(t, h, creatorID, true); res.Status != StatusSucceeded {
		t.Fatalf("full scan: %+v", res)
	}
	prof.pages = offsetPages(total)
	if res := mustRun(t, h, creatorID, false); res.Status != StatusSucceeded {
		t.Fatalf("incremental scan: %+v", res)
	}
	var deleted int
	if err := h.dbs.QueryRow(
		`SELECT COUNT(*) FROM works WHERE creator_id = ? AND deleted_at IS NOT NULL`, creatorID).Scan(&deleted); err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Fatalf("incremental scan soft-deleted %d works", deleted)
	}
}

// Enqueuer seam: creator-target subscriptions get all new works,
// collection-target subscriptions only the ones inside their collection, each
// at the subscription's quality.
func TestScanEnqueuesNewWorksBySubscription(t *testing.T) {
	// 40 works; 21..40 pre-seeded as known -> 20 new, of which 10 and 20 sit
	// in the collection.
	const total = 40
	prof := &fakeProvider{pages: offsetPages(total)}
	withMix(prof.pages, 10, "mix_1", "Fake合集")
	h := newHarness(t, prof)
	creatorID := insertCreator(t, h, "MS4wLjABAAAAfake_enqueue")
	prof.profile = &provider.Profile{SecUID: "x", Nickname: "Fake", AwemeCount: total}

	// Pre-seed: the collection and works 21..40 (40 of them inside the mix).
	res, err := h.dbs.Exec(
		`INSERT INTO collections (creator_id, mix_id, name, created_at) VALUES (?, 'mix_1', 'Fake合集', ?)`,
		creatorID, nowRFC3339())
	if err != nil {
		t.Fatal(err)
	}
	collectionID, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	for pos := 21; pos <= total; pos++ {
		it := fakeWork(pos)
		var coll any
		if pos%10 == 0 {
			coll = collectionID
		}
		if _, err := h.dbs.Exec(`
			INSERT INTO works (creator_id, collection_id, item_id, title, duration, published_at, created_at, updated_at)
			VALUES (?, ?, ?, ?, 30, ?, ?, ?)`,
			creatorID, coll, it.ItemID, it.Title, it.PublishedAt, nowRFC3339(), nowRFC3339()); err != nil {
			t.Fatal(err)
		}
	}
	for _, sub := range []struct {
		target  string
		coll    any
		quality string
	}{
		{"creator", nil, "720p"},
		{"collection", collectionID, "540p"},
	} {
		if _, err := h.dbs.Exec(`
			INSERT INTO subscriptions (target_type, creator_id, collection_id, interval_minutes,
			                           auto_download, quality, enabled, created_at)
			VALUES (?, ?, ?, 60, 1, ?, 1, ?)`,
			sub.target, creatorID, sub.coll, sub.quality, nowRFC3339()); err != nil {
			t.Fatal(err)
		}
	}

	var mu sync.Mutex
	type call struct {
		ids     []int64
		quality string
	}
	var calls []call
	h.sc.SetEnqueuer(enqueuerFunc(func(ctx context.Context, ids []int64, quality string) error {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, call{append([]int64(nil), ids...), quality})
		return nil
	}))

	result := mustRun(t, h, creatorID, true)
	if result.Status != StatusSucceeded || result.NewCount != 20 {
		t.Fatalf("scan = %+v, want succeeded with 20 new", result)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("enqueue calls = %d, want 2", len(calls))
	}
	// Creator subscription: all 20 new works at 720p.
	if len(calls[0].ids) != 20 || calls[0].quality != "720p" {
		t.Fatalf("creator enqueue = %d ids at %s, want 20 at 720p", len(calls[0].ids), calls[0].quality)
	}
	// Collection subscription: the 2 new multiples of 10 inside 1..20 at 540p.
	if len(calls[1].ids) != 2 || calls[1].quality != "540p" {
		t.Fatalf("collection enqueue = %d ids at %s, want 2 at 540p", len(calls[1].ids), calls[1].quality)
	}
	for _, id := range calls[1].ids {
		var itemID string
		if err := h.dbs.QueryRow(`SELECT item_id FROM works WHERE id = ?`, id).Scan(&itemID); err != nil {
			t.Fatal(err)
		}
		if itemID != "fake_0010" && itemID != "fake_0020" {
			t.Fatalf("collection enqueue contains %s (not in mix)", itemID)
		}
	}
}

// A nil Enqueuer (stage 4 default) must not fail the scan.
func TestScanNilEnqueuerSkips(t *testing.T) {
	prof := &fakeProvider{pages: offsetPages(20)}
	h := newHarness(t, prof)
	creatorID := insertCreator(t, h, "MS4wLjABAAAAfake_nilq")
	prof.profile = &provider.Profile{SecUID: "x", Nickname: "Fake", AwemeCount: 20}
	if res := mustRun(t, h, creatorID, true); res.Status != StatusSucceeded {
		t.Fatalf("scan = %+v", res)
	}
}

// Single-flight: a second Scan for the same creator returns the running scan
// id instead of starting a duplicate.
func TestScanSingleFlightDedup(t *testing.T) {
	release := make(chan struct{})
	prof := &fakeProvider{pages: map[string]*provider.PostsPage{
		"0": {Items: []provider.PostItem{fakeWork(1)}, HasMore: false},
	}}
	h := newHarness(t, prof)

	blocked := make(chan struct{}, 1)
	prof.postsHook = func(cursor string) {
		if cursor == "0" {
			select {
			case blocked <- struct{}{}:
			default:
			}
			<-release
		}
	}
	creatorID := insertCreator(t, h, "MS4wLjABAAAAfake_dedup")

	id1, err := h.sc.Scan(context.Background(), creatorID, true, TriggerManual)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("first scan did not reach the provider")
	}
	id2, err := h.sc.Scan(context.Background(), creatorID, true, TriggerManual)
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Fatalf("second Scan returned id %d, want the in-flight %d", id2, id1)
	}
	close(release)
	res := h.sc.WaitCreator(context.Background(), creatorID)
	if res.Status != StatusSucceeded {
		t.Fatalf("status = %s (%v)", res.Status, res.LastError)
	}
	if h.sc.IsScanning(creatorID) {
		t.Fatal("IsScanning true after completion")
	}
}

// RecycleStaleRuns marks rows left running by a previous process as failed.
func TestRecycleStaleRuns(t *testing.T) {
	prof := &fakeProvider{pages: offsetPages(20)}
	h := newHarness(t, prof)
	creatorID := insertCreator(t, h, "MS4wLjABAAAAfake_recycle")
	if _, err := h.dbs.Exec(
		`INSERT INTO scan_runs (creator_id, "trigger", full, status, started_at) VALUES (?, 'manual', 1, 'running', ?)`,
		creatorID, nowRFC3339()); err != nil {
		t.Fatal(err)
	}
	if err := h.sc.RecycleStaleRuns(); err != nil {
		t.Fatal(err)
	}
	var status, lastErr string
	if err := h.dbs.QueryRow(`SELECT status, last_error FROM scan_runs WHERE creator_id = ?`, creatorID).
		Scan(&status, &lastErr); err != nil {
		t.Fatal(err)
	}
	if status != StatusFailed || lastErr != "interrupted by restart" {
		t.Fatalf("recycled row = %s / %q", status, lastErr)
	}
}

type enqueuerFunc func(ctx context.Context, ids []int64, quality string) error

func (f enqueuerFunc) Enqueue(ctx context.Context, ids []int64, quality string) error {
	return f(ctx, ids, quality)
}
