package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"douyin/backend/internal/db"
)

func newTestService(t *testing.T) (*Service, *time.Time) {
	t.Helper()
	handle, err := db.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Migrate(handle); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { handle.Close() })

	svc := New(handle)
	// Deterministic clock for session/rate-limit assertions.
	current := time.Date(2026, 9, 11, 8, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return current }
	svc.limiter.now = func() time.Time { return current }
	return svc, &current
}

func TestSetupLoginLogoutFlow(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	if svc.Initialized(ctx) {
		t.Fatal("fresh service must not be initialized")
	}
	if err := svc.Setup(ctx, "admin", "hunter22"); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if !svc.Initialized(ctx) {
		t.Fatal("service must be initialized after setup")
	}
	if err := svc.Setup(ctx, "other", "hunter33"); err != ErrAlreadyInitialized {
		t.Fatalf("second setup: got %v, want ErrAlreadyInitialized", err)
	}

	// Wrong password -> ErrInvalidCredentials (and counted by the limiter).
	if _, err := svc.Login(ctx, "1.2.3.4", "admin", "wrong-password"); err != ErrInvalidCredentials {
		t.Fatalf("bad login: got %v, want ErrInvalidCredentials", err)
	}
	// Unknown user -> ErrInvalidCredentials.
	if _, err := svc.Login(ctx, "1.2.3.4", "nobody", "hunter22"); err != ErrInvalidCredentials {
		t.Fatalf("unknown user: got %v, want ErrInvalidCredentials", err)
	}

	token, err := svc.Login(ctx, "1.2.3.4", "admin", "hunter22")
	if err != nil {
		t.Fatalf("good login: %v", err)
	}
	if len(token) != 64 { // 32 random bytes, hex encoded
		t.Fatalf("token length = %d, want 64", len(token))
	}

	username, ok := svc.Validate(ctx, token)
	if !ok || username != "admin" {
		t.Fatalf("validate: got (%q, %v)", username, ok)
	}

	if err := svc.Logout(ctx, token); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if _, ok := svc.Validate(ctx, token); ok {
		t.Fatal("token must be invalid after logout")
	}
}

func TestSessionExpiryAndSliding(t *testing.T) {
	svc, clock := newTestService(t)
	ctx := context.Background()
	if err := svc.Setup(ctx, "admin", "hunter22"); err != nil {
		t.Fatalf("setup: %v", err)
	}

	token, err := svc.CreateSession(ctx, "admin")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// 6 days later the session is still valid...
	*clock = clock.Add(6 * 24 * time.Hour)
	if _, ok := svc.Validate(ctx, token); !ok {
		t.Fatal("session must survive 6 days")
	}
	// ...and validation slid the expiry 7 days from *now*.
	var expiresAt string
	if err := svc.db.QueryRow(`SELECT expires_at FROM sessions WHERE token = ?`, token).Scan(&expiresAt); err != nil {
		t.Fatalf("read expires_at: %v", err)
	}
	want := clock.Add(SessionTTL).UTC().Format(time.RFC3339)
	if expiresAt != want {
		t.Fatalf("sliding expiry = %s, want %s", expiresAt, want)
	}

	// Jump past the (slid) expiry: session must be rejected and deleted.
	*clock = clock.Add(8 * 24 * time.Hour)
	if _, ok := svc.Validate(ctx, token); ok {
		t.Fatal("expired session must be rejected")
	}
	var count int
	if err := svc.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&count); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if count != 0 {
		t.Fatalf("expired session not deleted (count=%d)", count)
	}
}

func TestLoginRateLimit(t *testing.T) {
	svc, clock := newTestService(t)
	ctx := context.Background()
	if err := svc.Setup(ctx, "admin", "hunter22"); err != nil {
		t.Fatalf("setup: %v", err)
	}

	for i := 0; i < 10; i++ {
		if _, err := svc.Login(ctx, "9.9.9.9", "admin", "wrongpw"); err != ErrInvalidCredentials {
			t.Fatalf("failure %d: got %v, want ErrInvalidCredentials", i, err)
		}
	}
	// Locked: even the correct password is refused.
	if _, err := svc.Login(ctx, "9.9.9.9", "admin", "hunter22"); err != ErrRateLimited {
		t.Fatalf("locked login: got %v, want ErrRateLimited", err)
	}
	// Other IPs are unaffected.
	if _, err := svc.Login(ctx, "8.8.8.8", "admin", "hunter22"); err != nil {
		t.Fatalf("other ip login should work: %v", err)
	}

	// After the 15-minute window the failures age out and the IP is unlocked.
	*clock = clock.Add(15*time.Minute + time.Second)
	if _, err := svc.Login(ctx, "9.9.9.9", "admin", "hunter22"); err != nil {
		t.Fatalf("login after lock expiry: %v", err)
	}

	// A successful login resets the failure counter.
	for i := 0; i < 5; i++ {
		_, _ = svc.Login(ctx, "7.7.7.7", "admin", "nope")
	}
	if _, err := svc.Login(ctx, "7.7.7.7", "admin", "hunter22"); err != nil {
		t.Fatalf("login with prior failures: %v", err)
	}
	for i := 0; i < 10; i++ { // 10 fresh failures now allowed again
		if _, err := svc.Login(ctx, "7.7.7.7", "admin", "nope"); err != ErrInvalidCredentials {
			t.Fatalf("post-reset failure %d: %v", i, err)
		}
	}
}

func TestChangePassword(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	if err := svc.Setup(ctx, "admin", "hunter22"); err != nil {
		t.Fatalf("setup: %v", err)
	}

	if err := svc.ChangePassword(ctx, "admin", "wrong", "newpass99"); err != ErrWrongOldPassword {
		t.Fatalf("wrong old password: got %v", err)
	}
	if err := svc.ChangePassword(ctx, "admin", "hunter22", "newpass99"); err != nil {
		t.Fatalf("change password: %v", err)
	}
	if _, err := svc.Login(ctx, "1.1.1.1", "admin", "hunter22"); err != ErrInvalidCredentials {
		t.Fatalf("old password must stop working: %v", err)
	}
	if _, err := svc.Login(ctx, "1.1.1.1", "admin", "newpass99"); err != nil {
		t.Fatalf("new password must work: %v", err)
	}
}

func TestMiddleware(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	if err := svc.Setup(ctx, "admin", "hunter22"); err != nil {
		t.Fatalf("setup: %v", err)
	}

	var gotUser string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser = UsernameFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	handler := svc.Middleware(inner)

	// No cookie -> 401 with the contract body.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/creators", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no cookie: status = %d, want 401", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["detail"] != "unauthorized" {
		t.Fatalf("401 body = %s", rec.Body.String())
	}

	// Invalid cookie -> 401.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/creators", nil)
	req.AddCookie(&http.Cookie{Name: CookieName, Value: "bogus"})
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bogus cookie: status = %d, want 401", rec.Code)
	}

	// Valid cookie -> 200, username injected, cookie re-set (sliding).
	token, err := svc.CreateSession(ctx, "admin")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/creators", nil)
	req.AddCookie(&http.Cookie{Name: CookieName, Value: token})
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid cookie: status = %d, want 200", rec.Code)
	}
	if gotUser != "admin" {
		t.Fatalf("ctx username = %q, want admin", gotUser)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != CookieName || cookies[0].Value != token || !cookies[0].HttpOnly {
		t.Fatalf("sliding cookie not re-set: %+v", cookies)
	}
}
