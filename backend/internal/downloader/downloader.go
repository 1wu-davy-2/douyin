// Package downloader implements the download queue (stage 5): a persistent
// SQLite-backed job table driven by an event-based worker pool.
//
// Design notes (fixing the legacy services.py disasters):
//
//   - The queue is the download_jobs table itself; the in-process dispatch
//     channel (capacity 64) only carries wake-up job ids. Restart recovery is
//     a single UPDATE (RecoverStale) plus a re-feed of queued ids - no polling.
//   - Eight claimant workers share a counting gate limited by the runtime
//     download_concurrency setting (default 3); PATCH /api/settings adjusts
//     the limit live without touching in-flight jobs.
//   - Every running job owns a cancellable context (map[jobID]cancelFunc), so
//     a user cancel aborts the exact transfer: the loop notices ctx.Err(),
//     marks the job canceled and removes the .part file. A process shutdown
//     (base ctx canceled) leaves the row as downloading on purpose; the next
//     start requeues it via RecoverStale.
//   - Progress is accumulated in memory and flushed by ONE dedicated writer
//     goroutine over ONE dedicated *sql.Conn at 1 Hz (legacy opened a SQLite
//     connection per 256 KiB chunk). SSE download.progress events are
//     published at the same 1 Hz cadence with a sliding-window speed estimate.
//   - A single shared http.Client serves every transfer (legacy built one
//     httpx client per video).
package downloader

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/http"
	"sync"
	"time"

	"douyin/backend/internal/events"
	"douyin/backend/internal/provider"
	"douyin/backend/internal/settings"
)

// Job statuses (download_jobs.status) - frozen by docs/api.md.
const (
	StatusQueued      = "queued"
	StatusDownloading = "downloading"
	StatusPausedQ     = "paused_q"
	StatusSucceeded   = "succeeded"
	StatusFailed      = "failed"
	StatusCanceled    = "canceled"
)

// defaults and tunables.
const (
	defaultConcurrency = 3
	workerCount        = 8  // fixed claimant pool; the gate enforces the real limit
	queueCapacity      = 64 // dispatch channel capacity
	flushInterval      = 1 * time.Second
	detailTimeout      = 90 * time.Second // provider.WorkDetail budget
	speedWindow        = 5 * time.Second  // sliding speed estimate window
	maxAttempts        = 5                // retry-failed ceiling
	// StopGrace is the default stop budget for finalizing in-flight jobs.
	StopGrace = 5 * time.Second
)

// settingsKeyPaused persists the queue pause flag across restarts.
const settingsKeyPaused = "queue_paused"

// ProviderSource supplies the effective provider per call. *provider.Resolver
// implements it; tests substitute fakes.
type ProviderSource interface {
	Resolve(ctx context.Context) provider.Provider
}

// Errors surfaced to the API layer (mapped to HTTP statuses there).
var (
	ErrNotFound = errors.New("download job not found")
	// ErrConflict marks state conflicts: deleting a downloading job, retrying
	// an active one, canceling a finished one.
	ErrConflict = errors.New("job state does not allow the operation")
	// ErrInvalid signals invalid arguments (bad quality, bad action, ...).
	ErrInvalid = errors.New("invalid argument")
)

// Deps wires the downloader.
type Deps struct {
	DB     *sql.DB
	Bus    *events.Bus
	Store  *settings.Store
	Source ProviderSource
	// DataDir anchors the DEFAULT download root <DataDir>/downloads (contract
	// v1.3): the creator-level creators.download_root override and the global
	// settings.download_root take precedence over it.
	DataDir string
	// BaseURL is the absolute http base used to resolve relative (mock) media
	// URLs such as "/mockcdn/{item}/{quality}.mp4". Empty leaves them as-is.
	BaseURL string
	// Client overrides the shared HTTP client (tests). Nil builds the default.
	Client *http.Client
	// Workers overrides the fixed claimant count (tests). 0 -> workerCount.
	Workers int
	// Mock disables inter-job pacing (mock CDN needs no rate limiting).
	Mock bool
}

// Downloader owns the job queue and its workers.
type Downloader struct {
	db      *sql.DB
	bus     *events.Bus
	store   *settings.Store
	src     ProviderSource
	dataDir string
	baseURL string
	client  *http.Client
	workers int
	mock    bool

	baseCtx   context.Context
	cancelAll context.CancelFunc

	gate *gate
	q    *jobQueue

	mu      sync.Mutex
	cancels map[int64]context.CancelFunc // running job -> cancel
	paused  bool
	started bool
	stopped bool

	conn     *sql.Conn // dedicated progress writer connection
	trackers map[int64]*tracker

	wg sync.WaitGroup // workers, flusher, feeders
}

// New builds a downloader; call RecoverStale + Start before use and Stop at
// shutdown. parent is canceled at process shutdown.
func New(parent context.Context, deps Deps) *Downloader {
	ctx, cancel := context.WithCancel(parent)
	workers := deps.Workers
	if workers <= 0 {
		workers = workerCount
	}
	client := deps.Client
	if client == nil {
		client = newHTTPClient()
	}
	return &Downloader{
		db:        deps.DB,
		bus:       deps.Bus,
		store:     deps.Store,
		src:       deps.Source,
		dataDir:   deps.DataDir,
		baseURL:   deps.BaseURL,
		client:    client,
		workers:   workers,
		mock:      deps.Mock,
		baseCtx:   ctx,
		cancelAll: cancel,
		gate:      newGate(defaultConcurrency),
		q:         newJobQueue(queueCapacity),
		cancels:   make(map[int64]context.CancelFunc),
		trackers:  make(map[int64]*tracker),
	}
}

// SetBaseURL overrides the media URL base (tests wire it to the httptest
// server after the port is known). Must be called before Start.
func (d *Downloader) SetBaseURL(u string) { d.baseURL = u }

// SetConcurrency adjusts the live worker limit (wired to the
// download_concurrency setting change hook). 1..8, other values ignored.
func (d *Downloader) SetConcurrency(n int) {
	if n < 1 || n > 8 {
		return
	}
	d.gate.setLimit(n)
}

// Concurrency reports the current worker limit.
func (d *Downloader) Concurrency() int { return d.gate.Limit() }

// Paused reports whether dispatch is paused.
func (d *Downloader) Paused() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.paused
}

// RecoverStale requeues download_jobs rows left in status=downloading by a
// previous process (crash or shutdown): downloading -> queued. Returns the
// number of recovered rows.
func (d *Downloader) RecoverStale() (int64, error) {
	res, err := d.db.Exec(`
		UPDATE download_jobs SET status = ?, error = 'recovered after restart'
		WHERE status = ?`, StatusQueued, StatusDownloading)
	if err != nil {
		return 0, fmt.Errorf("downloader: recover stale jobs: %w", err)
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		log.Printf("[downloader] recovered %d stale downloading job(s) as queued", n)
	}
	return n, nil
}

// Start reads the persisted queue state, spawns the worker pool, the progress
// flusher and re-feeds every queued job id into the dispatch channel.
// Idempotent.
func (d *Downloader) Start() error {
	d.mu.Lock()
	if d.started {
		d.mu.Unlock()
		return nil
	}
	d.started = true
	d.mu.Unlock()

	// Effective concurrency from settings (default 3).
	if d.store != nil {
		if view, err := d.store.View(d.baseCtx); err == nil && view.DownloadConcurrency > 0 {
			d.gate.setLimit(view.DownloadConcurrency)
		} else if err != nil {
			log.Printf("[downloader] read download_concurrency (%v), using %d", err, defaultConcurrency)
		}
	}

	// Persisted pause flag.
	if paused, err := d.loadPausedFlag(); err != nil {
		log.Printf("[downloader] read paused flag: %v", err)
	} else if paused {
		d.mu.Lock()
		d.paused = true
		d.mu.Unlock()
		log.Printf("[downloader] queue starts paused")
	}

	// Dedicated single connection for progress writes (the legacy bug fix:
	// one persistent writer instead of a connection per chunk).
	conn, err := d.db.Conn(context.Background())
	if err != nil {
		return fmt.Errorf("downloader: acquire progress connection: %w", err)
	}
	d.mu.Lock()
	d.conn = conn
	d.mu.Unlock()

	for i := 0; i < d.workers; i++ {
		d.wg.Add(1)
		go d.worker()
	}
	d.wg.Add(1)
	go d.flushLoop()

	d.feedQueued()
	return nil
}

// Stop shuts the queue down: cancels all running jobs (their rows are left in
// status=downloading on purpose so the next RecoverStale requeues them),
// closes the dispatch channel and waits up to timeout for workers/flusher to
// finalize. Safe to call multiple times.
func (d *Downloader) Stop(timeout time.Duration) {
	d.mu.Lock()
	if d.stopped {
		d.mu.Unlock()
		return
	}
	d.stopped = true
	d.mu.Unlock()

	d.cancelAll() // loops wind down: shutdown cancel != user cancel
	d.q.close()

	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		log.Printf("[downloader] stop timeout (%s): %d job(s) may not have finalized",
			timeout, d.runningCount())
	}

	if conn := d.detachConn(); conn != nil {
		conn.Close()
	}
}

// -------------------------------------------------------------- internals --

// worker claims job ids from the dispatch channel until it closes.
func (d *Downloader) worker() {
	defer d.wg.Done()
	for id := range d.q.ch {
		// Gentle pacing: douyin throttles bursts of detail+CDN requests with
		// transient 403s; a small stagger between job starts avoids the wave.
		if !d.mock {
			time.Sleep(time.Duration(800+rand.Intn(1200)) * time.Millisecond)
		}
		d.runJob(id)
	}
}

// feedQueued re-feeds every queued job id (startup / resume). Runs in the
// background so a full channel cannot block the caller; stops when the queue
// closes.
func (d *Downloader) feedQueued() {
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		rows, err := d.db.Query(`SELECT id FROM download_jobs WHERE status = ? ORDER BY id`, StatusQueued)
		if err != nil {
			log.Printf("[downloader] feed queued: %v", err)
			return
		}
		ids := make([]int64, 0, 16)
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			log.Printf("[downloader] feed queued: %v", err)
			return
		}
		for _, id := range ids {
			if !d.q.push(id) {
				return // queue closed (shutdown)
			}
		}
		if len(ids) > 0 {
			log.Printf("[downloader] re-fed %d queued job(s)", len(ids))
		}
	}()
}

// registerCancel stores the job's cancel func; fails after Stop.
func (d *Downloader) registerCancel(jobID int64, cancel context.CancelFunc) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped {
		return false
	}
	d.cancels[jobID] = cancel
	return true
}

func (d *Downloader) unregisterCancel(jobID int64) {
	d.mu.Lock()
	delete(d.cancels, jobID)
	d.mu.Unlock()
}

func (d *Downloader) cancelFuncFor(jobID int64) context.CancelFunc {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cancels[jobID]
}

func (d *Downloader) runningCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.cancels)
}

func (d *Downloader) isPaused() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.paused
}

func (d *Downloader) setPaused(v bool) {
	d.mu.Lock()
	d.paused = v
	d.mu.Unlock()
}

func (d *Downloader) detachConn() *sql.Conn {
	d.mu.Lock()
	defer d.mu.Unlock()
	conn := d.conn
	d.conn = nil
	return conn
}

// ------------------------------------------------------------- paused flag --

func (d *Downloader) loadPausedFlag() (bool, error) {
	var raw string
	err := d.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, settingsKeyPaused).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var b bool
	if json.Unmarshal([]byte(raw), &b) == nil {
		return b, nil
	}
	return raw == "true", nil
}

func (d *Downloader) storePausedFlag(v bool) error {
	raw := "false"
	if v {
		raw = "true"
	}
	_, err := d.db.Exec(`
		INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, settingsKeyPaused, raw)
	if err != nil {
		return fmt.Errorf("downloader: persist paused flag: %w", err)
	}
	return nil
}

// ------------------------------------------------------------------- gate --

// gate is a reconfigurable counting semaphore (the download_concurrency
// limit). Waiters wake on any release or limit change.
type gate struct {
	mu    sync.Mutex
	wake  chan struct{} // closed and replaced on every state change
	limit int
	held  int
}

func newGate(limit int) *gate {
	return &gate{wake: make(chan struct{}), limit: limit}
}

// acquire blocks until a slot is free or ctx is done.
func (g *gate) acquire(ctx context.Context) bool {
	for {
		g.mu.Lock()
		if g.held < g.limit {
			g.held++
			g.mu.Unlock()
			return true
		}
		w := g.wake // grabbed in the same critical section as the check
		g.mu.Unlock()
		select {
		case <-w:
		case <-ctx.Done():
			return false
		}
	}
}

func (g *gate) release() {
	g.mu.Lock()
	if g.held > 0 {
		g.held--
	}
	close(g.wake)
	g.wake = make(chan struct{})
	g.mu.Unlock()
}

func (g *gate) setLimit(n int) {
	g.mu.Lock()
	g.limit = n
	close(g.wake)
	g.wake = make(chan struct{})
	g.mu.Unlock()
}

func (g *gate) Limit() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.limit
}

// -------------------------------------------------------------- job queue --

// jobQueue is the dispatch channel of job ids. The SQLite download_jobs table
// is the real queue state; this channel only wakes workers up.
type jobQueue struct {
	mu     sync.Mutex
	ch     chan int64
	closed bool
}

func newJobQueue(capacity int) *jobQueue {
	return &jobQueue{ch: make(chan int64, capacity)}
}

// push enqueues a job id. Holds the mutex while sending so close can never
// race a send (close waits for the in-flight send to complete). Returns false
// when the queue is closed.
func (q *jobQueue) push(id int64) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return false
	}
	q.ch <- id
	return true
}

func (q *jobQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.closed = true
	close(q.ch)
}

// ------------------------------------------------------------------ client --

// newHTTPClient builds the single shared download client (legacy built one
// httpx client per video). No overall timeout: the per-job ctx governs.
func newHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConns:          16,
			MaxIdleConnsPerHost:   4,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
		},
	}
}

// publishStatus emits the state-class download.status event.
func (d *Downloader) publishStatus(jobID, workID int64, status string, errStr *string) {
	if d.bus == nil {
		return
	}
	d.bus.Publish(events.Event{
		Type: events.TypeDownloadStatus,
		Data: events.DownloadStatus{JobID: jobID, Status: status, Error: errStr, WorkID: workID},
	})
}

func strPtr(v string) *string { return &v }

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }
