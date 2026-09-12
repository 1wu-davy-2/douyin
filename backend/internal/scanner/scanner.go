// Package scanner implements the archive scanner (stage 4): cursor-paged
// fetching with strict bad-page handling, cursor-fallback pagination,
// completeness reconciliation and soft deletion.
//
// It fixes the three root causes of the legacy "502 works on the profile but
// only 400+ scanned" bug:
//
//  1. Bad pages (provider error / missing fields) are retried with exponential
//     backoff on the SAME cursor and never silently swallowed; consecutive bad
//     pages terminate the run with status=partial instead of "success".
//  2. Cursor pagination never gives up while it still makes progress: when
//     next_cursor is empty or repeats while has_more=true, a fallback cursor
//     (oldest seen published_at of the segment, in ms, minus 1) is
//     synthesized; fallbacks are unlimited and a repeated cursor with fresh
//     works is NOT a termination condition (the legacy code broke out of the
//     loop on the first repeated cursor).
//  3. After every run the library count is reconciled against the profile's
//     reported_work_count; a gap above the threshold forces status=partial so
//     the scheduler can automatically re-run a full scan.
package scanner

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"douyin/backend/internal/events"
	"douyin/backend/internal/provider"
	"douyin/backend/internal/settings"
)

// Scan run statuses (scan_runs.status) and triggers (scan_runs.trigger).
const (
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusPartial   = "partial"
	StatusFailed    = "failed"

	TriggerManual    = "manual"
	TriggerScheduled = "scheduled"
)

// Enqueuer is the download-queue seam consumed by the scanner after a scan
// finds new works under auto_download subscriptions. Stage 5 injects the real
// implementation via SetEnqueuer; until then the field stays nil and matching
// works are logged and skipped.
type Enqueuer interface {
	// Enqueue queues the given works for download at the given quality.
	Enqueue(ctx context.Context, workIDs []int64, quality string) error
}

// ProviderSource supplies the effective provider per call. *provider.Resolver
// implements this signature; tests can substitute fakes.
type ProviderSource interface {
	Resolve(ctx context.Context) provider.Provider
}

// ErrCreatorNotFound is returned by Scan when the creator row does not exist.
var ErrCreatorNotFound = errors.New("creator not found")

// Result is the outcome of a finished scan run.
type Result struct {
	ScanID       int64   `json:"scan_id"`
	CreatorID    int64   `json:"creator_id"`
	Status       string  `json:"status"`
	Trigger      string  `json:"trigger"`
	Full         bool    `json:"full"`
	Pages        int     `json:"pages"`
	NewCount     int     `json:"new_count"`
	UpdatedCount int     `json:"updated_count"`
	EmptyPages   int     `json:"empty_pages"`
	Completeness int     `json:"completeness"`
	LastError    *string `json:"last_error"`

	// NewWorkIDs lists the ids of works first seen in this scan (input for
	// subscription auto-download matching). Not serialized.
	NewWorkIDs []int64 `json:"-"`
}

// activeScan tracks one in-flight (or queued) scan per creator.
type activeScan struct {
	id     int64
	done   chan struct{} // closed exactly once when the run finished
	result Result        // written before done is closed
}

// Scanner owns scan runs. Only one scan executes at any time (global
// semaphore); a Scan request for a creator that already has an active scan
// returns the existing scan id instead of starting a duplicate.
type Scanner struct {
	src      ProviderSource
	db       *sql.DB
	bus      *events.Bus
	store    *settings.Store
	enqueuer Enqueuer

	baseCtx context.Context // canceled at process shutdown

	// Scan concurrency limit is read dynamically from settings
	// (scan_concurrency, 1-5, default 3). running is guarded by mu; waiters
	// re-check the limit periodically via scanWakeup/timer.
	running    int
	scanWakeup chan struct{}

	mu     sync.Mutex
	active map[int64]*activeScan

	wg sync.WaitGroup // running run() goroutines

	// sleep pauses for d and returns early on ctx cancellation. Tests replace
	// it to record backoff/delay durations without real waiting.
	sleep func(ctx context.Context, d time.Duration) error
}

// New wires the scanner. baseCtx must be canceled at process shutdown so
// in-flight scans can finalize (status=failed) before the database closes.
func New(baseCtx context.Context, src ProviderSource, handle *sql.DB, bus *events.Bus, store *settings.Store) *Scanner {
	return &Scanner{
		src:     src,
		db:      handle,
		bus:     bus,
		store:   store,
		baseCtx:    baseCtx,
		scanWakeup: make(chan struct{}, 1),
		active:     make(map[int64]*activeScan),
		sleep:   sleepCtx,
	}
}

// SetEnqueuer wires the download queue (stage 5 injection point). Call before
// scans start; not safe concurrently with a running scan.
func (s *Scanner) SetEnqueuer(e Enqueuer) { s.enqueuer = e }

// scanLimit reads the configured scan concurrency (settings.scan_concurrency,
// clamped 1-5, default 3) at call time, so a settings PATCH takes effect for
// queued waiters without restart.
func (s *Scanner) scanLimit() int {
	limit := 3
	if s.store != nil {
		if view, err := s.store.View(s.baseCtx); err == nil && view.ScanConcurrency > 0 {
			limit = view.ScanConcurrency
		}
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 5 {
		limit = 5
	}
	return limit
}

// acquireSlot blocks until a scan slot is available or ctx is done. Waiters
// wake on release or on a 500ms timer (which also picks up limit changes).
func (s *Scanner) acquireSlot(ctx context.Context) error {
	for {
		s.mu.Lock()
		if s.running < s.scanLimit() {
			s.running++
			s.mu.Unlock()
			return nil
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.scanWakeup:
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// releaseSlot frees a scan slot and wakes one waiter.
func (s *Scanner) releaseSlot() {
	s.mu.Lock()
	s.running--
	s.mu.Unlock()
	select {
	case s.scanWakeup <- struct{}{}:
	default:
	}
}

// Scan starts a scan run for creatorID and returns its scan id immediately
// (the run continues in the background). trigger is "manual" or "scheduled".
//
// Single-flight: only one scan executes process-wide. If the same creator
// already has an active (or queued) scan, its existing scan id is returned;
// scans for other creators are accepted and queue behind the running one.
func (s *Scanner) Scan(ctx context.Context, creatorID int64, full bool, trigger string) (int64, error) {
	s.mu.Lock()
	if a, ok := s.active[creatorID]; ok {
		id := a.id
		s.mu.Unlock()
		log.Printf("[scanner] creator %d already has scan %d in flight, returning it", creatorID, id)
		return id, nil
	}

	var secUID string
	err := s.db.QueryRowContext(ctx, `SELECT sec_uid FROM creators WHERE id = ?`, creatorID).Scan(&secUID)
	if errors.Is(err, sql.ErrNoRows) {
		s.mu.Unlock()
		return 0, ErrCreatorNotFound
	}
	if err != nil {
		s.mu.Unlock()
		return 0, fmt.Errorf("scanner: load creator %d: %w", creatorID, err)
	}

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO scan_runs (creator_id, "trigger", full, status, started_at) VALUES (?, ?, ?, ?, ?)`,
		creatorID, trigger, boolToInt(full), StatusRunning, nowRFC3339())
	if err != nil {
		s.mu.Unlock()
		return 0, fmt.Errorf("scanner: create scan run: %w", err)
	}
	scanID, err := res.LastInsertId()
	if err != nil {
		s.mu.Unlock()
		return 0, fmt.Errorf("scanner: scan run id: %w", err)
	}

	a := &activeScan{id: scanID, done: make(chan struct{})}
	s.active[creatorID] = a
	s.wg.Add(1)
	s.mu.Unlock()

	go s.run(a, creatorID, secUID, full, trigger)
	return scanID, nil
}

// ScanSync starts a scan and blocks until it finishes, returning the final
// result. Used by the scheduler (API endpoints use the async Scan).
func (s *Scanner) ScanSync(ctx context.Context, creatorID int64, full bool, trigger string) (Result, error) {
	if _, err := s.Scan(ctx, creatorID, full, trigger); err != nil {
		return Result{}, err
	}
	return s.WaitCreator(ctx, creatorID), nil
}

// WaitCreator blocks until the creator's active scan (if any) finishes and
// returns its result; when nothing is active the latest recorded run is
// returned.
func (s *Scanner) WaitCreator(ctx context.Context, creatorID int64) Result {
	s.mu.Lock()
	a := s.active[creatorID]
	s.mu.Unlock()
	if a == nil {
		return s.latestResult(ctx, creatorID)
	}
	select {
	case <-a.done:
		s.mu.Lock()
		res := a.result
		s.mu.Unlock()
		if res.ScanID != 0 {
			return res
		}
		return s.latestResult(ctx, creatorID)
	case <-ctx.Done():
		return Result{ScanID: a.id, CreatorID: creatorID, Status: StatusRunning}
	}
}

// IsScanning reports whether a scan for creatorID is queued or running.
func (s *Scanner) IsScanning(creatorID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.active[creatorID]
	return ok
}

// WaitIdle blocks until all in-flight scans finished or the timeout elapsed.
func (s *Scanner) WaitIdle(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		log.Printf("[scanner] %d scan(s) still active after %s", len(s.activeSnapshot()), timeout)
	}
}

func (s *Scanner) activeSnapshot() map[int64]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[int64]int64, len(s.active))
	for id, a := range s.active {
		out[id] = a.id
	}
	return out
}

// run executes the scan and publishes its bookkeeping. It always terminates
// with a finalized scan_runs row and a scan.done event, even on cancellation.
func (s *Scanner) run(a *activeScan, creatorID int64, secUID string, full bool, trigger string) {
	defer s.wg.Done()
	res := s.execute(a.id, creatorID, secUID, full, trigger)

	a.result = res
	s.mu.Lock()
	delete(s.active, creatorID)
	s.mu.Unlock()
	close(a.done)
}

// latestResult reads the most recent scan_runs row of a creator.
func (s *Scanner) latestResult(ctx context.Context, creatorID int64) Result {
	res := Result{CreatorID: creatorID, Status: StatusRunning}
	var lastErr sql.NullString
	var full int
	err := s.db.QueryRowContext(ctx, `
		SELECT id, status, pages, new_count, updated_count, empty_pages, completeness,
		       last_error, "trigger", full
		FROM scan_runs WHERE creator_id = ? ORDER BY id DESC LIMIT 1`, creatorID).
		Scan(&res.ScanID, &res.Status, &res.Pages, &res.NewCount, &res.UpdatedCount,
			&res.EmptyPages, &res.Completeness, &lastErr, &res.Trigger, &full)
	if err != nil {
		return res
	}
	res.Full = full != 0
	if lastErr.Valid {
		v := lastErr.String
		res.LastError = &v
	}
	return res
}

// RecycleStaleRuns marks scan runs left in status=running by a previous
// process as failed. Called once at startup.
func (s *Scanner) RecycleStaleRuns() error {
	res, err := s.db.Exec(`
		UPDATE scan_runs SET status = ?, last_error = ?, finished_at = ?
		WHERE status = ?`, StatusFailed, "interrupted by restart", nowRFC3339(), StatusRunning)
	if err != nil {
		return fmt.Errorf("scanner: recycle stale runs: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("[scanner] recycled %d stale running scan run(s) as failed", n)
	}
	return nil
}

// ------------------------------------------------------------------ helpers --

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func strPtr(v string) *string { return &v }

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// sleepCtx is the production sleeper: real time, aborting on ctx cancellation.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
