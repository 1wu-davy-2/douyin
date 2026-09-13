package settings

// MinIO settings tests: view defaults, write-only secret masking, patch
// validation (endpoint required when enabled, concurrency bounds) and prefix
// normalization.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMinioViewDefaults(t *testing.T) {
	store, _ := newTestStore(t)

	view, err := store.View(t.Context())
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	m := view.Minio
	if m.Enabled {
		t.Error("minio must default to disabled")
	}
	if m.Endpoint != "" || m.Bucket != "" || m.AccessKey != "" {
		t.Errorf("minio endpoints must default empty: %+v", m)
	}
	if m.Prefix != DefaultMinioPrefix {
		t.Errorf("default prefix = %q, want %q", m.Prefix, DefaultMinioPrefix)
	}
	if m.Concurrency != DefaultMinioConcurrency {
		t.Errorf("default concurrency = %d, want %d", m.Concurrency, DefaultMinioConcurrency)
	}
	if m.SecretSet {
		t.Error("secret_set must default to false")
	}
	if m.UseSSL {
		t.Error("use_ssl must default to false")
	}
}

func TestMinioPatchValidationAndMasking(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := t.Context()

	// Enabling without an endpoint is rejected (nothing stored).
	if _, err := store.Apply(ctx, Patch{Minio: &MinioPatch{Enabled: ptr(true)}}); err == nil {
		t.Fatal("minio.enabled=true without an endpoint must be rejected")
	}
	if view, _ := store.View(ctx); view.Minio.Enabled {
		t.Fatal("rejected patch must not have been stored")
	}

	// Concurrency bounds.
	if _, err := store.Apply(ctx, Patch{Minio: &MinioPatch{Concurrency: ptr(0)}}); err == nil {
		t.Fatal("minio.concurrency=0 must be rejected")
	}
	if _, err := store.Apply(ctx, Patch{Minio: &MinioPatch{Concurrency: ptr(9)}}); err == nil {
		t.Fatal("minio.concurrency=9 must be rejected")
	}

	// A full valid patch round-trips and the secret is masked.
	if _, err := store.Apply(ctx, Patch{Minio: &MinioPatch{
		Enabled:     ptr(true),
		Endpoint:    ptr("127.0.0.1:9000"),
		Bucket:      ptr("douyin"),
		AccessKey:   ptr("AKIAMINIO"),
		SecretKey:   ptr("s3cr3t-secret"),
		UseSSL:      ptr(true),
		Prefix:      ptr("media"),
		Concurrency: ptr(4),
	}}); err != nil {
		t.Fatalf("apply valid minio patch: %v", err)
	}
	view, err := store.View(ctx)
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	m := view.Minio
	if !m.Enabled || m.Endpoint != "127.0.0.1:9000" || m.Bucket != "douyin" ||
		m.AccessKey != "AKIAMINIO" || !m.SecretSet || !m.UseSSL ||
		m.Prefix != "media/" || m.Concurrency != 4 {
		t.Errorf("minio view = %+v, want the stored values", m)
	}
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal view: %v", err)
	}
	if strings.Contains(string(raw), "s3cr3t-secret") {
		t.Error("minio secret leaked in the settings view JSON")
	}

	// Clearing the endpoint while still enabled is rejected.
	if _, err := store.Apply(ctx, Patch{Minio: &MinioPatch{Endpoint: ptr("  ")}}); err == nil {
		t.Fatal("clearing minio.endpoint while enabled must be rejected")
	}

	// Prefix normalization: leading slashes stripped, trailing slash ensured.
	if _, err := store.Apply(ctx, Patch{Minio: &MinioPatch{Prefix: ptr("/nested/path/")}}); err != nil {
		t.Fatalf("apply prefix: %v", err)
	}
	if view, _ = store.View(ctx); view.Minio.Prefix != "nested/path/" {
		t.Errorf("normalized prefix = %q, want nested/path/", view.Minio.Prefix)
	}

	// Disabling first, then clearing the endpoint is allowed.
	if _, err := store.Apply(ctx, Patch{Minio: &MinioPatch{Enabled: ptr(false)}}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := store.Apply(ctx, Patch{Minio: &MinioPatch{Endpoint: ptr("")}}); err != nil {
		t.Fatalf("clear endpoint after disable: %v", err)
	}
}

func TestMinioConfigForUploader(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := t.Context()

	// Defaults when unset.
	cfg := store.MinioConfig(ctx)
	if cfg.Enabled || cfg.Ready() {
		t.Errorf("default config must be disabled/incomplete: %+v", cfg)
	}
	if cfg.Prefix != DefaultMinioPrefix || cfg.Concurrency != DefaultMinioConcurrency {
		t.Errorf("default config = %+v", cfg)
	}

	if _, err := store.Apply(ctx, Patch{Minio: &MinioPatch{
		Enabled:   ptr(true),
		Endpoint:  ptr("nas.local:9000"),
		Bucket:    ptr("douyin"),
		AccessKey: ptr("ak"),
		SecretKey: ptr("topsecret"),
	}}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	cfg = store.MinioConfig(ctx)
	if !cfg.Enabled || !cfg.Ready() {
		t.Errorf("config must be enabled/ready: %+v", cfg)
	}
	if cfg.SecretKey != "topsecret" {
		t.Errorf("uploader-facing secret = %q, want the stored value", cfg.SecretKey)
	}
	if cfg.Prefix != DefaultMinioPrefix {
		t.Errorf("prefix = %q, want the default", cfg.Prefix)
	}

	// An explicitly cleared prefix (empty string) is preserved, not defaulted.
	if _, err := store.Apply(ctx, Patch{Minio: &MinioPatch{Prefix: ptr("")}}); err != nil {
		t.Fatalf("clear prefix: %v", err)
	}
	if got := store.MinioPrefix(ctx); got != "" {
		t.Errorf("cleared prefix = %q, want empty", got)
	}
}

func TestMinioConcurrencyHookFires(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := t.Context()

	called := make(chan int, 1)
	store.SetOnMinioConcurrency(func(n int) { called <- n })

	if _, err := store.Apply(ctx, Patch{Minio: &MinioPatch{Concurrency: ptr(3)}}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	select {
	case n := <-called:
		if n != 3 {
			t.Errorf("hook got %d, want 3", n)
		}
	default:
		t.Fatal("minio_concurrency hook did not fire")
	}

	// An unrelated patch must not fire the hook.
	if _, err := store.Apply(ctx, Patch{Minio: &MinioPatch{Bucket: ptr("other")}}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	select {
	case n := <-called:
		t.Errorf("hook fired unexpectedly with %d", n)
	default:
	}
}
