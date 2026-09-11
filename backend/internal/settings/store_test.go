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
	if err := db.Migrate(handle); err != nil {
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
