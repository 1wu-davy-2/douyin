// Package auth implements administrator bootstrap, session management and the
// request-authentication middleware.
//
// Credentials live in the settings table under the internal keys
// "admin_username" / "admin_password_hash" (never exposed by the settings
// API). Sessions are 32-byte random tokens stored in the sessions table and
// carried in the HttpOnly cookie "dy_session" with a 7-day sliding expiry.
// Login attempts are rate-limited per client IP: 10 failures within a 15
// minute window lock that IP out for 15 minutes.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// CookieName is the session cookie name from the frozen contract.
const CookieName = "dy_session"

// SessionTTL is the sliding session lifetime.
const SessionTTL = 7 * 24 * time.Hour

// Settings keys used for admin credentials (internal, not API-visible).
const (
	keyAdminUsername     = "admin_username"
	keyAdminPasswordHash = "admin_password_hash"
)

// Sentinel errors the API layer maps to HTTP responses.
var (
	ErrAlreadyInitialized = errors.New("admin already initialized")
	ErrNotInitialized     = errors.New("admin not initialized")
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrRateLimited        = errors.New("too many failed attempts")
	ErrWrongOldPassword   = errors.New("old password incorrect")
)

type ctxKeyUser struct{}

// Service implements authentication on top of the SQLite database.
type Service struct {
	db      *sql.DB
	limiter *loginLimiter

	// now is swappable for tests.
	now func() time.Time
}

// New creates the auth service for an (already migrated) database.
func New(db *sql.DB) *Service {
	return &Service{db: db, limiter: newLoginLimiter(time.Now), now: time.Now}
}

// ---------------------------------------------------------------- settings --

func (s *Service) getSetting(ctx context.Context, key string) (string, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("auth: read setting %s: %w", key, err)
	}
	var decoded string
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		// Tolerate legacy raw (non-JSON) values.
		return raw, nil
	}
	return decoded, nil
}

func (s *Service) setSetting(ctx context.Context, key, value string) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("auth: encode setting %s: %w", key, err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO settings(key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, string(encoded))
	if err != nil {
		return fmt.Errorf("auth: write setting %s: %w", key, err)
	}
	return nil
}

// ----------------------------------------------------------- admin bootstrap --

// Initialized reports whether the administrator account exists.
func (s *Service) Initialized(ctx context.Context) bool {
	hash, err := s.getSetting(ctx, keyAdminPasswordHash)
	return err == nil && hash != ""
}

// Setup creates the administrator account. It fails if one already exists.
func (s *Service) Setup(ctx context.Context, username, password string) error {
	username = strings.TrimSpace(username)
	if username == "" {
		return errors.New("username must not be empty")
	}
	if len(password) < 6 {
		return errors.New("password must be at least 6 characters")
	}
	if s.Initialized(ctx) {
		return ErrAlreadyInitialized
	}
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	if err := s.setSetting(ctx, keyAdminUsername, username); err != nil {
		return err
	}
	if err := s.setSetting(ctx, keyAdminPasswordHash, hash); err != nil {
		return err
	}
	return nil
}

// ------------------------------------------------------------------ sessions --

// newToken returns a 32-byte random session token (hex-encoded).
func newToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: generate token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// CreateSession persists a fresh 7-day session for username and returns the
// token to store in the cookie.
func (s *Service) CreateSession(ctx context.Context, username string) (string, error) {
	token, err := newToken()
	if err != nil {
		return "", err
	}
	expires := s.now().Add(SessionTTL).UTC().Format(time.RFC3339)
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions(token, username, expires_at) VALUES (?, ?, ?)`,
		token, username, expires); err != nil {
		return "", fmt.Errorf("auth: insert session: %w", err)
	}
	return token, nil
}

// Validate returns the username for a valid, unexpired token. It also slides
// the expiry forward (7 days from now), which is what makes the session
// "sliding"; the caller should refresh the cookie in the response.
func (s *Service) Validate(ctx context.Context, token string) (string, bool) {
	if token == "" {
		return "", false
	}
	var username, expiresAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT username, expires_at FROM sessions WHERE token = ?`, token).
		Scan(&username, &expiresAt)
	if err != nil {
		return "", false
	}
	expires, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil || !s.now().Before(expires) {
		// Expired (or unparsable): drop it.
		_, _ = s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token = ?`, token)
		return "", false
	}
	newExpiry := s.now().Add(SessionTTL).UTC().Format(time.RFC3339)
	if newExpiry != expiresAt {
		_, _ = s.db.ExecContext(ctx,
			`UPDATE sessions SET expires_at = ? WHERE token = ?`, newExpiry, token)
	}
	return username, true
}

// Logout removes a session; missing sessions are not an error.
func (s *Service) Logout(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token = ?`, token); err != nil {
		return fmt.Errorf("auth: delete session: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------- login etc --

// Login verifies credentials and returns a fresh session token. The client IP
// is rate-limited: after loginMaxFailures failures inside loginWindow the IP
// is locked (ErrRateLimited) until the failures age out of the window.
func (s *Service) Login(ctx context.Context, ip, username, password string) (string, error) {
	if !s.limiter.Allowed(ip) {
		return "", ErrRateLimited
	}

	storedName, err := s.getSetting(ctx, keyAdminUsername)
	if err != nil {
		return "", err
	}
	storedHash, err := s.getSetting(ctx, keyAdminPasswordHash)
	if err != nil {
		return "", err
	}
	if storedHash == "" {
		return "", ErrNotInitialized
	}

	userOK := secureEqual(strings.TrimSpace(username), storedName)
	passOK := CheckPassword(password, storedHash)
	if !userOK || !passOK {
		s.limiter.RecordFailure(ip)
		return "", ErrInvalidCredentials
	}

	s.limiter.Reset(ip)
	return s.CreateSession(ctx, storedName)
}

// ChangePassword verifies the old password and replaces the stored hash.
func (s *Service) ChangePassword(ctx context.Context, username, oldPassword, newPassword string) error {
	if len(newPassword) < 6 {
		return errors.New("new password must be at least 6 characters")
	}
	storedHash, err := s.getSetting(ctx, keyAdminPasswordHash)
	if err != nil {
		return err
	}
	if storedHash == "" {
		return ErrNotInitialized
	}
	if !CheckPassword(oldPassword, storedHash) {
		return ErrWrongOldPassword
	}
	hash, err := HashPassword(newPassword)
	if err != nil {
		return err
	}
	return s.setSetting(ctx, keyAdminPasswordHash, hash)
}

// secureEqual compares two strings in constant time.
func secureEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// -------------------------------------------------------------- rate limiting --

const (
	loginWindow   = 15 * time.Minute
	loginMaxFails = 10
)

// loginLimiter is the in-memory per-IP failure tracker:
// 10 failures within a 15-minute window lock the IP for 15 minutes
// (i.e. until its oldest failure leaves the window).
type loginLimiter struct {
	mu    sync.Mutex
	fails map[string][]time.Time
	now   func() time.Time
}

func newLoginLimiter(now func() time.Time) *loginLimiter {
	return &loginLimiter{fails: make(map[string][]time.Time), now: now}
}

func (l *loginLimiter) Allowed(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := l.now().Add(-loginWindow)
	kept := l.fails[ip][:0]
	for _, ts := range l.fails[ip] {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	if len(kept) == 0 {
		delete(l.fails, ip)
		return true
	}
	l.fails[ip] = kept
	return len(kept) < loginMaxFails
}

func (l *loginLimiter) RecordFailure(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fails[ip] = append(l.fails[ip], l.now())
}

func (l *loginLimiter) Reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, ip)
}

// ------------------------------------------------------------------ passwords --

// HashPassword hashes with bcrypt.
func HashPassword(password string) (string, error) {
	if password == "" {
		return "", errors.New("password must not be empty")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("auth: bcrypt hash: %w", err)
	}
	return string(hash), nil
}

// CheckPassword reports whether password matches the bcrypt hash. Unknown or
// malformed hashes simply fail (never panic).
func CheckPassword(password, hash string) bool {
	if hash == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// ------------------------------------------------------------------ middleware --

// Middleware authenticates the request via the dy_session cookie. On success
// it injects the username into the request context and slides both the stored
// expiry and the cookie forward. Otherwise it responds 401
// {"detail":"unauthorized"} per the contract.
func (s *Service) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(CookieName)
		if err == nil {
			if username, ok := s.Validate(r.Context(), cookie.Value); ok {
				http.SetCookie(w, &http.Cookie{
					Name:     CookieName,
					Value:    cookie.Value,
					Path:     "/",
					HttpOnly: true,
					SameSite: http.SameSiteLaxMode,
					MaxAge:   int(SessionTTL / time.Second),
				})
				ctx := context.WithValue(r.Context(), ctxKeyUser{}, username)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
		}
		WriteUnauthorized(w)
	})
}

// WriteUnauthorized answers with the contract's 401 body.
func WriteUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"detail":"unauthorized"}` + "\n"))
}

// SetSessionCookie writes the session cookie on login/setup.
func SetSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(SessionTTL / time.Second),
	})
}

// ClearSessionCookie expires the session cookie on logout.
func ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// UsernameFrom returns the authenticated username injected by Middleware.
func UsernameFrom(ctx context.Context) string {
	username, _ := ctx.Value(ctxKeyUser{}).(string)
	return username
}
