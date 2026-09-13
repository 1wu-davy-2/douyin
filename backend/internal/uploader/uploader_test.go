package uploader

// Tests run the full queue machinery against an in-memory objectStore double
// (real MinIO is not reachable from the suite): enqueue + upload, idempotent
// skip, retry-then-failure counting, bounded-queue drops and the disabled /
// incomplete-config no-op paths.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"douyin/backend/internal/config"
	"douyin/backend/internal/db"
	"douyin/backend/internal/settings"
)

// ---------------------------------------------------------------- mem store --

const testBucket = "douyin"

// memStore is the in-memory objectStore: it records call counts so tests can
// assert idempotent skips and retry counts, and injects stat/put failures.
type memStore struct {
	mu        sync.Mutex
	objects   map[string]int64
	statCalls int
	putCalls  int
	puts      []string
	statErr   error
	putErr    error
}

func newMemStore() *memStore { return &memStore{objects: map[string]int64{}} }

func (m *memStore) BucketExists(_ context.Context, bucket string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return bucket == testBucket, nil
}

func (m *memStore) StatObject(_ context.Context, _, key string) (int64, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.statCalls++
	if m.statErr != nil {
		return 0, false, m.statErr
	}
	size, ok := m.objects[key]
	return size, ok, nil
}

func (m *memStore) PutObject(_ context.Context, _, key, localPath string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.putCalls++
	m.puts = append(m.puts, key)
	if m.putErr != nil {
		return m.putErr
	}
	data, err := os.ReadFile(localPath)
	if err != nil {
		return err
	}
	m.objects[key] = int64(len(data))
	return nil
}

func (m *memStore) has(key string, size int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.objects[key]
	return ok && v == size
}

func (m *memStore) countPuts() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.putCalls
}

// ----------------------------------------------------------------- harness --

func newTestSettingsStore(t *testing.T) *settings.Store {
	t.Helper()
	handle, err := db.Open(filepath.Join(t.TempDir(), "uploader.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Migrate(handle, t.TempDir()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { handle.Close() })
	return settings.NewStore(handle, config.Settings{DataDir: t.TempDir()})
}

// enableMinio stores a complete, enabled MinIO configuration.
func enableMinio(t *testing.T, store *settings.Store) {
	t.Helper()
	enabled := true
	endpoint := "minio.local:9000"
	bucket := testBucket
	if _, err := store.Apply(t.Context(), settings.Patch{Minio: &settings.MinioPatch{
		Enabled:  &enabled,
		Endpoint: &endpoint,
		Bucket:   &bucket,
	}}); err != nil {
		t.Fatalf("apply minio settings: %v", err)
	}
}

// newTestUploader builds an uploader wired to the given memStore with a fast
// retry backoff, stopping it automatically at test end.
func newTestUploader(t *testing.T, store *settings.Store, capacity int, ms *memStore) *Uploader {
	t.Helper()
	u := newUploader(store, capacity, func(Config) (objectStore, error) { return ms, nil })
	u.backoff = time.Millisecond
	t.Cleanup(func() { u.Stop(2 * time.Second) })
	return u
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func writeTempFile(t *testing.T, size int64) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "video.mp4")
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	return path
}

// ------------------------------------------------------------------ tests --

func TestEnqueueUploadsObject(t *testing.T) {
	store := newTestSettingsStore(t)
	enableMinio(t, store)
	ms := newMemStore()
	u := newTestUploader(t, store, queueCapacity, ms)
	u.Start()

	local := writeTempFile(t, 128)
	u.Enqueue([]UploadItem{{LocalPath: local, ObjectKey: "douyin/sec1/item1/video.mp4", SizeBytes: 128}})

	waitFor(t, "object stored", func() bool { return ms.has("douyin/sec1/item1/video.mp4", 128) })
	s := u.Status(t.Context())
	if s.UploadedTotal != 1 || s.FailedTotal != 0 || s.DroppedTotal != 0 {
		t.Errorf("status = %+v, want uploaded=1 failed=0 dropped=0", s)
	}
}

func TestIdempotentSkipSameSize(t *testing.T) {
	store := newTestSettingsStore(t)
	enableMinio(t, store)
	ms := newMemStore()
	const key = "douyin/sec1/item1/video.mp4"
	ms.objects[key] = 128 // remote already holds the object at the same size
	u := newTestUploader(t, store, queueCapacity, ms)
	u.Start()

	local := writeTempFile(t, 128)
	u.Enqueue([]UploadItem{{LocalPath: local, ObjectKey: key, SizeBytes: 128}})

	waitFor(t, "skip recorded", func() bool { return u.Status(t.Context()).UploadedTotal == 1 })
	if n := ms.countPuts(); n != 0 {
		t.Errorf("idempotent skip must not PUT, got %d put(s)", n)
	}
}

func TestSizeMismatchOverwrites(t *testing.T) {
	store := newTestSettingsStore(t)
	enableMinio(t, store)
	ms := newMemStore()
	const key = "douyin/sec1/item1/video.mp4"
	ms.objects[key] = 5 // stale remote object with a different size
	u := newTestUploader(t, store, queueCapacity, ms)
	u.Start()

	local := writeTempFile(t, 128)
	u.Enqueue([]UploadItem{{LocalPath: local, ObjectKey: key, SizeBytes: 128}})

	waitFor(t, "overwrite stored", func() bool { return ms.has(key, 128) })
	s := u.Status(t.Context())
	if s.UploadedTotal != 1 || s.FailedTotal != 0 {
		t.Errorf("status = %+v, want uploaded=1 failed=0", s)
	}
}

func TestRetriesThenRecordsFailure(t *testing.T) {
	store := newTestSettingsStore(t)
	enableMinio(t, store)
	ms := newMemStore()
	ms.putErr = errors.New("boom")
	u := newTestUploader(t, store, queueCapacity, ms)
	u.Start()

	local := writeTempFile(t, 128)
	u.Enqueue([]UploadItem{{LocalPath: local, ObjectKey: "douyin/sec1/item1/video.mp4", SizeBytes: 128}})

	waitFor(t, "failure recorded", func() bool { return u.Status(t.Context()).FailedTotal == 1 })
	if n := ms.countPuts(); n != 1+maxRetries {
		t.Errorf("put attempts = %d, want %d (1 try + %d retries)", n, 1+maxRetries, maxRetries)
	}
	s := u.Status(t.Context())
	if s.UploadedTotal != 0 {
		t.Errorf("uploaded_total = %d, want 0", s.UploadedTotal)
	}
	if !strings.Contains(s.LastError, "boom") {
		t.Errorf("last_error = %q, want it to mention the cause", s.LastError)
	}
}

func TestQueueFullDrops(t *testing.T) {
	store := newTestSettingsStore(t)
	enableMinio(t, store)
	ms := newMemStore()
	// Capacity 1 and no Start: nothing drains the queue.
	u := newTestUploader(t, store, 1, ms)

	local := writeTempFile(t, 8)
	u.Enqueue([]UploadItem{
		{LocalPath: local, ObjectKey: "douyin/sec1/item1/a.mp4", SizeBytes: 8},
		{LocalPath: local, ObjectKey: "douyin/sec1/item1/b.mp4", SizeBytes: 8},
	})

	s := u.Status(t.Context())
	if s.Queued != 1 {
		t.Errorf("queued = %d, want 1", s.Queued)
	}
	if s.DroppedTotal != 1 {
		t.Errorf("dropped_total = %d, want 1", s.DroppedTotal)
	}
	u.Stop(time.Second)
}

func TestDisabledOrIncompleteIsNoOp(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T, store *settings.Store)
	}{
		{"disabled", func(t *testing.T, store *settings.Store) {
			disabled := false
			if _, err := store.Apply(t.Context(), settings.Patch{Minio: &settings.MinioPatch{Enabled: &disabled}}); err != nil {
				t.Fatalf("apply: %v", err)
			}
		}},
		{"missing endpoint", func(t *testing.T, store *settings.Store) {
			enabled := true // enabled but no endpoint stored: incomplete
			if _, err := store.Apply(t.Context(), settings.Patch{Minio: &settings.MinioPatch{Enabled: &enabled}}); err == nil {
				t.Fatal("enabling without endpoint must be rejected")
			}
		}},
		{"missing bucket", func(t *testing.T, store *settings.Store) {
			enabled := true
			endpoint := "minio.local:9000"
			if _, err := store.Apply(t.Context(), settings.Patch{Minio: &settings.MinioPatch{
				Enabled: &enabled, Endpoint: &endpoint,
			}}); err != nil {
				t.Fatalf("apply: %v", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestSettingsStore(t)
			tc.prepare(t, store)
			ms := newMemStore()
			u := newTestUploader(t, store, queueCapacity, ms)
			u.Start()

			local := writeTempFile(t, 8)
			u.Enqueue([]UploadItem{{LocalPath: local, ObjectKey: "douyin/sec1/item1/a.mp4", SizeBytes: 8}})

			s := u.Status(t.Context())
			if s.Queued != 0 || s.UploadedTotal != 0 || s.FailedTotal != 0 {
				t.Errorf("status = %+v, want a complete no-op", s)
			}
			if n := ms.countPuts(); n != 0 {
				t.Errorf("no-op must not reach the store, got %d put(s)", n)
			}
		})
	}
}

func TestObjectKeyRule(t *testing.T) {
	if got := ObjectKey("douyin/", "MS4wLjABAAAAxx", "7361122", "title.mp4"); got != "douyin/MS4wLjABAAAAxx/7361122/title.mp4" {
		t.Errorf("ObjectKey = %q", got)
	}
	if got := ObjectKey("douyin/", "", "7361122", "metadata.json"); got != "douyin/nosec/7361122/metadata.json" {
		t.Errorf("ObjectKey with empty sec_uid = %q, want the nosec fallback", got)
	}
}

func TestTestConnection(t *testing.T) {
	store := newTestSettingsStore(t)
	enableMinio(t, store)
	ms := newMemStore()
	u := newTestUploader(t, store, queueCapacity, ms)

	// The configured bucket exists -> ok.
	if err := u.TestConnection(t.Context()); err != nil {
		t.Errorf("test connection with existing bucket: %v", err)
	}

	// A configured but missing bucket must fail with a clear message.
	bucket := "missing"
	if _, err := store.Apply(t.Context(), settings.Patch{Minio: &settings.MinioPatch{Bucket: &bucket}}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	err := u.TestConnection(t.Context())
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("missing bucket error = %v, want \"does not exist\"", err)
	}

	// Incomplete configuration fails before dialing: disable, then clear the
	// endpoint (clearing while enabled is rejected by the settings store).
	disabled := false
	if _, err := store.Apply(t.Context(), settings.Patch{Minio: &settings.MinioPatch{Enabled: &disabled}}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	empty := ""
	if _, err := store.Apply(t.Context(), settings.Patch{Minio: &settings.MinioPatch{Endpoint: &empty}}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	err = u.TestConnection(t.Context())
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Errorf("incomplete config error = %v, want \"not configured\"", err)
	}
}

func TestSetConcurrencyClamps(t *testing.T) {
	store := newTestSettingsStore(t)
	enableMinio(t, store)
	u := newTestUploader(t, store, queueCapacity, newMemStore())

	u.SetConcurrency(4)
	if lim := u.gate.limit; lim != 4 {
		t.Errorf("limit = %d, want 4", lim)
	}
	u.SetConcurrency(99)
	if lim := u.gate.limit; lim != maxConcurrency {
		t.Errorf("limit = %d, want clamped to %d", lim, maxConcurrency)
	}
	u.SetConcurrency(0)
	if lim := u.gate.limit; lim != 1 {
		t.Errorf("limit = %d, want clamped to 1", lim)
	}
}
