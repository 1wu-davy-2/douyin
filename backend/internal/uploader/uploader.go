// Package uploader implements the fire-and-forget MinIO sync (docs/MINIO_PLAN.md
// option A): files of succeeded downloads are pushed onto a bounded in-memory
// queue and uploaded by a small worker pool.
//
// The sync is strictly best-effort: it never blocks the downloader and never
// influences local job state. Uploads are idempotent (StatObject + size
// compare), failures are retried with exponential backoff and surfaced through
// Status (settings page sync state). Storage operations sit behind the
// objectStore interface so tests exercise the full queue/retry machinery
// against an in-memory implementation.
package uploader

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"douyin/backend/internal/settings"
)

// Tunables.
const (
	// queueCapacity bounds the in-memory queue; overflow is dropped and
	// counted (a slow/broken MinIO must never pile memory up).
	queueCapacity = 128
	// uploadWorkers is the fixed claimant pool; the gate enforces the real
	// minio_concurrency limit (same architecture as the downloader).
	uploadWorkers = 8
	// maxRetries is the number of retries after the first attempt
	// (backoff 2s/4s/8s).
	maxRetries = 3
	// defaultBackoff is the first retry delay, doubled per retry.
	defaultBackoff = 2 * time.Second
	// perObjectTimeout bounds one upload attempt cycle (stat + put).
	perObjectTimeout = 10 * time.Minute
	// maxConcurrency matches the download_concurrency style 1..8 bound.
	maxConcurrency = 8
)

// UploadItem is one local file to mirror into MinIO.
type UploadItem struct {
	LocalPath string // absolute local file path
	ObjectKey string // remote key: {prefix}{sec_uid}/{item_id}/{filename}
	SizeBytes int64  // local size (idempotency check against the remote object)
}

// Status is the GET /api/settings/minio/status projection.
type Status struct {
	Queued        int    `json:"queued"`
	UploadedTotal int64  `json:"uploaded_total"`
	FailedTotal   int64  `json:"failed_total"`
	DroppedTotal  int64  `json:"dropped_total"`
	LastError     string `json:"last_error"`
}

// storeFactory builds an objectStore for one task. Built per task so
// endpoint/key changes in the settings page take effect immediately (the
// alternative - a cached client invalidated on config change - buys nothing
// here; client construction is cheap relative to an upload).
type storeFactory func(cfg Config) (objectStore, error)

// Uploader owns the upload queue and its workers. Create with New, spawn the
// pool with Start, wire SetConcurrency to the minio_concurrency settings hook
// and call Stop at shutdown to drain the queue.
type Uploader struct {
	store   *settings.Store
	factory storeFactory
	backoff time.Duration

	q    *uploadQueue
	gate *concurrencyGate

	mu       sync.Mutex
	uploaded int64
	failed   int64
	dropped  int64
	lastErr  string

	started sync.Once
	wg      sync.WaitGroup
}

// New builds an uploader over the settings store.
func New(store *settings.Store) *Uploader {
	return newUploader(store, queueCapacity, newMinioStore)
}

func newUploader(store *settings.Store, capacity int, factory storeFactory) *Uploader {
	return &Uploader{
		store:   store,
		factory: factory,
		backoff: defaultBackoff,
		q:       newUploadQueue(capacity),
		gate:    newConcurrencyGate(settings.DefaultMinioConcurrency),
	}
}

// Start spawns the worker pool (idempotent). The pool is fixed; the initial
// gate limit comes from the minio_concurrency setting.
func (u *Uploader) Start() {
	u.started.Do(func() {
		if u.store != nil {
			if cfg := u.store.MinioConfig(context.Background()); cfg.Concurrency >= 1 {
				u.gate.setLimit(cfg.Concurrency)
			}
		}
		for i := 0; i < uploadWorkers; i++ {
			u.wg.Add(1)
			go u.worker()
		}
	})
}

// Stop closes the queue and waits up to timeout for the workers to drain it.
// In-flight uploads keep running on a background context; on timeout the wait
// is abandoned (leftovers are simply dropped - the process is shutting down).
// Safe to call multiple times.
func (u *Uploader) Stop(timeout time.Duration) {
	u.q.close()
	done := make(chan struct{})
	go func() {
		u.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		log.Printf("[uploader] stop timeout (%s): %d item(s) left queued", timeout, len(u.q.ch))
	}
}

// SetConcurrency adjusts the live upload limit (wired to the
// minio_concurrency setting change hook). Values outside 1..8 are clamped.
func (u *Uploader) SetConcurrency(n int) { u.gate.setLimit(n) }

// Enabled reports whether MinIO sync is switched on and configured enough to
// talk to a server (endpoint + bucket). Read live from the settings store.
func (u *Uploader) Enabled(ctx context.Context) bool {
	if u == nil || u.store == nil {
		return false
	}
	cfg := u.store.MinioConfig(ctx)
	return cfg.Enabled && cfg.Ready()
}

// Prefix returns the configured object key prefix (trailing slash included,
// default "douyin/").
func (u *Uploader) Prefix(ctx context.Context) string {
	if u == nil || u.store == nil {
		return settings.DefaultMinioPrefix
	}
	return u.store.MinioConfig(ctx).Prefix
}

// ObjectKey renders the remote key {prefix}{sec_uid}/{item_id}/{filename}
// (docs/MINIO_PLAN.md): anchoring on sec_uid/item_id keeps keys stable when a
// work is renamed; media servers read titles from the synced metadata.json.
func ObjectKey(prefix, secUID, itemID, filename string) string {
	if strings.TrimSpace(secUID) == "" {
		secUID = "nosec"
	}
	return prefix + secUID + "/" + itemID + "/" + filename
}

// TestConnection dials the currently configured MinIO endpoint and verifies
// the bucket exists (settings page test button, same pattern as the SMTP test
// mail). The bucket is NOT created: the user builds it explicitly.
func (u *Uploader) TestConnection(ctx context.Context) error {
	if u == nil || u.store == nil {
		return fmt.Errorf("minio sync is not wired")
	}
	cfg := u.store.MinioConfig(ctx)
	if !cfg.Ready() {
		return fmt.Errorf("minio is not configured: endpoint and bucket are required")
	}
	st, err := u.factory(asStoreConfig(cfg))
	if err != nil {
		return err
	}
	ok, err := st.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return fmt.Errorf("bucket %q check: %w", cfg.Bucket, err)
	}
	if !ok {
		return fmt.Errorf("bucket %q does not exist (create it in MinIO first)", cfg.Bucket)
	}
	return nil
}

// Enqueue best-effort offers items for upload. It never blocks and never
// returns an error: disabled or incomplete configuration is a no-op, a full
// (or stopped) queue drops and counts items. Callers treat this as
// fire-and-forget - the local job is already succeeded when this runs.
func (u *Uploader) Enqueue(items []UploadItem) {
	if u == nil || len(items) == 0 {
		return
	}
	if !u.Enabled(context.Background()) {
		return
	}
	for _, item := range items {
		if item.LocalPath == "" || item.ObjectKey == "" {
			continue
		}
		if !u.q.push(item) {
			u.countDropped()
		}
	}
}

// Status reports the queue counters for the settings page. Queued is the
// channel length (approximate: items already claimed by workers waiting for a
// gate slot are not counted). ctx is reserved for future remote queries.
func (u *Uploader) Status(ctx context.Context) Status {
	_ = ctx
	if u == nil {
		return Status{}
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return Status{
		Queued:        len(u.q.ch),
		UploadedTotal: u.uploaded,
		FailedTotal:   u.failed,
		DroppedTotal:  u.dropped,
		LastError:     u.lastErr,
	}
}

// ------------------------------------------------------------- internals --

// worker drains the queue until it closes. Uploads run on background
// contexts: a process shutdown does not abort in-flight transfers, Stop's
// drain window governs.
func (u *Uploader) worker() {
	defer u.wg.Done()
	for item := range u.q.ch {
		u.gate.acquire()
		u.process(item)
		u.gate.release()
	}
}

// process uploads one item: fail-fast local stat, per-task store, then a
// stat(+skip)/put round retried up to maxRetries times with exponential
// backoff. Any failure is recorded and swallowed - the queue has no
// dead-letter, the local file stays the source of truth.
func (u *Uploader) process(item UploadItem) {
	cfg := u.store.MinioConfig(context.Background())
	if !cfg.Ready() { // disabled mid-flight or incomplete
		u.recordFailure(fmt.Errorf("upload %s: minio disabled or incomplete", item.ObjectKey))
		return
	}
	fi, err := os.Stat(item.LocalPath)
	if err != nil {
		// A vanished local file will not heal by retrying: fail immediately.
		u.recordFailure(fmt.Errorf("upload %s: stat local file: %w", item.ObjectKey, err))
		return
	}
	size := fi.Size()

	st, err := u.factory(asStoreConfig(cfg))
	if err != nil {
		u.recordFailure(fmt.Errorf("upload %s: %w", item.ObjectKey, err))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), perObjectTimeout)
	defer cancel()
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			delay := u.backoff << (attempt - 1) // 2s, 4s, 8s
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				u.recordFailure(fmt.Errorf("upload %s: %w", item.ObjectKey, ctx.Err()))
				return
			}
		}
		skipped, err := uploadOnce(ctx, st, cfg.Bucket, item, size)
		if err == nil {
			u.recordSuccess(item, skipped)
			return
		}
		lastErr = err
	}
	u.recordFailure(fmt.Errorf("upload %s: %w", item.ObjectKey, lastErr))
}

// uploadOnce performs one stat(+skip)/put round: an existing object with the
// same size means the item is already synced (idempotent skip), anything else
// falls through to a full PUT (overwrites a size mismatch too).
func uploadOnce(ctx context.Context, st objectStore, bucket string, item UploadItem, size int64) (skipped bool, err error) {
	remote, found, err := st.StatObject(ctx, bucket, item.ObjectKey)
	if err != nil {
		return false, fmt.Errorf("stat object: %w", err)
	}
	if found && remote == size {
		return true, nil
	}
	return false, st.PutObject(ctx, bucket, item.ObjectKey, item.LocalPath)
}

// recordSuccess / recordFailure / countDropped keep the Status counters.
// Every outcome is logged: the sync must stay observable without a UI.

func (u *Uploader) recordSuccess(item UploadItem, skipped bool) {
	u.mu.Lock()
	u.uploaded++
	u.mu.Unlock()
	if skipped {
		log.Printf("[uploader] %s already synced (same size), skipped", item.ObjectKey)
		return
	}
	log.Printf("[uploader] synced %s (%d bytes)", item.ObjectKey, item.SizeBytes)
}

func (u *Uploader) recordFailure(err error) {
	u.mu.Lock()
	u.failed++
	u.lastErr = err.Error()
	u.mu.Unlock()
	log.Printf("[uploader] %v", err)
}

func (u *Uploader) countDropped() {
	u.mu.Lock()
	u.dropped++
	u.mu.Unlock()
	log.Printf("[uploader] queue full: upload item dropped")
}

// ----------------------------------------------------------- upload queue --

// uploadQueue is the bounded item channel. push never blocks (full or closed
// -> false) and close can never race a send (both hold the mutex).
type uploadQueue struct {
	mu     sync.Mutex
	ch     chan UploadItem
	closed bool
}

func newUploadQueue(capacity int) *uploadQueue {
	return &uploadQueue{ch: make(chan UploadItem, capacity)}
}

func (q *uploadQueue) push(item UploadItem) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return false
	}
	select {
	case q.ch <- item:
		return true
	default:
		return false // full: caller drops and counts
	}
}

func (q *uploadQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.closed = true
	close(q.ch)
}

// ------------------------------------------------------- concurrency gate --

// concurrencyGate is a reconfigurable counting semaphore (the live
// minio_concurrency limit). Waiters wake on release or limit change; there is
// no context because workers only exit via queue close (Stop drains).
type concurrencyGate struct {
	mu    sync.Mutex
	cond  *sync.Cond
	limit int
	held  int
}

func newConcurrencyGate(limit int) *concurrencyGate {
	g := &concurrencyGate{limit: limit}
	g.cond = sync.NewCond(&g.mu)
	return g
}

func (g *concurrencyGate) acquire() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for g.held >= g.limit {
		g.cond.Wait()
	}
	g.held++
}

func (g *concurrencyGate) release() {
	g.mu.Lock()
	if g.held > 0 {
		g.held--
	}
	g.cond.Signal()
	g.mu.Unlock()
}

// setLimit clamps to 1..maxConcurrency and wakes every waiter.
func (g *concurrencyGate) setLimit(n int) {
	if n < 1 {
		n = 1
	}
	if n > maxConcurrency {
		n = maxConcurrency
	}
	g.mu.Lock()
	g.limit = n
	g.cond.Broadcast()
	g.mu.Unlock()
}
