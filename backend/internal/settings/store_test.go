package settings

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"douyin/backend/internal/config"
	"douyin/backend/internal/db"
)

func newTestStore(t *testing.T) (*Store, config.Settings) {
	t.Helper()
	cfg := config.Settings{DataDir: t.TempDir(), SidecarIdleTimeout: 10 * time.Minute}
	handle, err := db.Open(filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Migrate(handle, t.TempDir()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { handle.Close() })
	return NewStore(handle, cfg), cfg
}

func TestViewDefaultsAndMasking(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := t.Context()

	view, err := store.View(ctx)
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	if view.ProviderMode != ModeAuto {
		t.Errorf("default provider_mode = %q, want auto", view.ProviderMode)
	}
	if view.DownloadConcurrency != 3 || view.DownloadQuality != "1080p" {
		t.Errorf("defaults wrong: %+v", view)
	}
	if view.Cookie != "" {
		t.Errorf("empty cookie must render as empty, got %q", view.Cookie)
	}
	if view.SMTP.PasswordSet {
		t.Error("password_set must default to false")
	}
}

func TestApplyValidatesAndPersists(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := t.Context()

	// Invalid provider_mode rejected.
	if _, err := store.Apply(ctx, Patch{ProviderMode: ptr("bogus")}); err == nil {
		t.Fatal("invalid provider_mode must be rejected")
	}
	// Invalid concurrency rejected.
	bad := 9
	if _, err := store.Apply(ctx, Patch{DownloadConcurrency: &bad}); err == nil {
		t.Fatal("download_concurrency=9 must be rejected")
	}
	// Valid patch applied.
	good := 5
	changed, err := store.Apply(ctx, Patch{
		ProviderMode:        ptr(ModeMock),
		DownloadConcurrency: &good,
		Cookie:              ptr("SESSDATA=abcdef123456789; ttwid=xyz"),
		NotifyOnNewWork:     ptr(true),
		SMTP:                &SMTPPatch{Host: ptr("smtp.example.com"), Port: ptr(465), Password: ptr("s3cret")},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !changed {
		t.Fatal("cookie change must be reported")
	}

	view, err := store.View(ctx)
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	if view.ProviderMode != ModeMock || view.DownloadConcurrency != 5 || !view.NotifyOnNewWork {
		t.Fatalf("patch not persisted: %+v", view)
	}
	// Cookie masked to 8 chars + "..."; password not echoed.
	if view.Cookie != "SESSDATA"+"..." {
		t.Errorf("masked cookie = %q, want first 8 chars + ...", view.Cookie)
	}
	if view.SMTP.Host != "smtp.example.com" || view.SMTP.Port != 465 {
		t.Errorf("smtp merge wrong: %+v", view.SMTP)
	}
	if !view.SMTP.PasswordSet {
		t.Error("password_set must be true after setting a password")
	}

	// Full SMTP settings (internal) include the password.
	smtpCfg := store.SMTPSettings(ctx)
	if smtpCfg.Password != "s3cret" {
		t.Errorf("smtp password = %q, want s3cret", smtpCfg.Password)
	}

	// Short cookies are fully masked.
	if _, err := store.Apply(ctx, Patch{Cookie: ptr("short")}); err != nil {
		t.Fatalf("apply short cookie: %v", err)
	}
	view, _ = store.View(ctx)
	if view.Cookie != "..." {
		t.Errorf("short cookie masking = %q, want ...", view.Cookie)
	}

	// Setting the same cookie again reports no change.
	changed, err = store.Apply(ctx, Patch{Cookie: ptr("short")})
	if err != nil || changed {
		t.Fatalf("unchanged cookie: changed=%v err=%v", changed, err)
	}
}

func TestCookieFileWritten(t *testing.T) {
	store, cfg := newTestStore(t)
	ctx := t.Context()

	if _, err := store.Apply(ctx, Patch{Cookie: ptr("cookie-value-123")}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(cfg.DataDir, ".cookie"))
	if err != nil {
		t.Fatalf("read cookie file: %v", err)
	}
	if string(content) != "cookie-value-123" {
		t.Fatalf("cookie file = %q", string(content))
	}
}

func TestIdleTimeoutHook(t *testing.T) {
	store, _ := newTestStore(t)
	called := make(chan time.Duration, 1)
	store.SetOnSidecarIdleTimeout(func(d time.Duration) { called <- d })

	if _, err := store.Apply(t.Context(), Patch{SidecarIdleTimeoutMin: ptr(3)}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	select {
	case d := <-called:
		if d != 3*time.Minute {
			t.Fatalf("hook got %v, want 3m", d)
		}
	case <-time.After(time.Second):
		t.Fatal("idle timeout hook not called")
	}
}

func ptr[T any](v T) *T { return &v }

// Contract v1.3: the global download_root setting. Default is empty in the
// view (meaning <data_dir>/downloads), a relative path is rejected, a valid
// absolute path is created on disk, and an empty patch clears it back.
func TestDownloadRootPatchValidationAndDefault(t *testing.T) {
	store, cfg := newTestStore(t)
	ctx := t.Context()

	view, err := store.View(ctx)
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	if view.DownloadRoot != "" {
		t.Fatalf("default view download_root = %q, want empty", view.DownloadRoot)
	}
	if got := store.EffectiveDownloadRoot(ctx); got != filepath.Join(cfg.DataDir, "downloads") {
		t.Fatalf("default effective root = %q, want %q", got, filepath.Join(cfg.DataDir, "downloads"))
	}

	// A relative path is rejected and nothing is stored.
	if _, err := store.Apply(ctx, Patch{DownloadRoot: ptr("relative/root")}); err == nil {
		t.Fatal("relative download_root must be rejected")
	}

	// A valid absolute path is cleaned, stored and created (nested dirs).
	target := filepath.Join(cfg.DataDir, "media", "root")
	if _, err := store.Apply(ctx, Patch{DownloadRoot: ptr(target)}); err != nil {
		t.Fatalf("apply download_root: %v", err)
	}
	if fi, err := os.Stat(target); err != nil || !fi.IsDir() {
		t.Fatalf("download_root %s not created: %v", target, err)
	}
	if view, err = store.View(ctx); err != nil {
		t.Fatalf("view: %v", err)
	}
	if view.DownloadRoot != filepath.Clean(target) {
		t.Fatalf("view download_root = %q, want %q", view.DownloadRoot, filepath.Clean(target))
	}
	if got := store.EffectiveDownloadRoot(ctx); got != filepath.Clean(target) {
		t.Fatalf("effective root = %q, want %q", got, filepath.Clean(target))
	}

	// Whitespace-only clears the override back to the default.
	if _, err := store.Apply(ctx, Patch{DownloadRoot: ptr("   ")}); err != nil {
		t.Fatalf("clear download_root: %v", err)
	}
	if view, err = store.View(ctx); err != nil {
		t.Fatalf("view: %v", err)
	}
	if view.DownloadRoot != "" {
		t.Fatalf("view download_root after clear = %q, want empty", view.DownloadRoot)
	}
	if got := store.EffectiveDownloadRoot(ctx); got != filepath.Join(cfg.DataDir, "downloads") {
		t.Fatalf("effective root after clear = %q, want the default", got)
	}
}
