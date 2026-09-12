// Package settings implements the runtime settings store: values live in the
// settings table (JSON-encoded) and override the environment-derived defaults.
// It backs GET/PATCH /api/settings, writes the cookie file for the Python
// sidecar on cookie changes, and owns the notification (SMTP test) helper.
//
// Sensitive values: the cookie is masked to its first 8 characters + "..."
// when returned over the API; the SMTP password is never echoed at all.
package settings

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"douyin/backend/internal/config"
	"douyin/backend/internal/db"
)

// Allowed provider modes.
const (
	ModeAuto    = "auto"
	ModeSidecar = "sidecar"
	ModeMock    = "mock"
)

// SMTP is the full SMTP configuration (including the password) for the
// notification sender; it never crosses the API boundary.
type SMTP struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
	To       string
}

// Store reads/writes the runtime settings.
type Store struct {
	db  *sql.DB
	cfg config.Settings

	// onSidecarIdleTimeout, when set, is called after a successful change of
	// sidecar_idle_timeout_minutes so the running sidecar manager adjusts.
	onSidecarIdleTimeout func(time.Duration)

	// onDownloadConcurrency, when set, is called after a successful change of
	// download_concurrency so the running downloader resizes its worker gate.
	onDownloadConcurrency func(int)
}

// NewStore creates the store over a migrated database.
func NewStore(handle *sql.DB, cfg config.Settings) *Store {
	return &Store{db: handle, cfg: cfg}
}

// SetOnSidecarIdleTimeout wires the sidecar-manager notification hook.
func (s *Store) SetOnSidecarIdleTimeout(fn func(time.Duration)) {
	s.onSidecarIdleTimeout = fn
}

// SetOnDownloadConcurrency wires the downloader notification hook.
func (s *Store) SetOnDownloadConcurrency(fn func(int)) {
	s.onDownloadConcurrency = fn
}

// CookieFilePath is where the Douyin cookie is exported for the sidecar
// (contract: <data_dir>/.cookie, hot-read by the sidecar on every request).
func (s *Store) CookieFilePath() string {
	return filepath.Join(s.cfg.DataDir, ".cookie")
}

// raw returns the stored values keyed by setting key (values are JSON
// strings). Missing rows simply don't appear in the map.
func (s *Store) raw(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, fmt.Errorf("settings: read: %w", err)
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("settings: scan: %w", err)
		}
		out[k] = v
	}
	return out, rows.Err()
}

// ProviderMode returns the effective provider mode setting with the documented
// default "auto". Errors are logged and treated as the default — health and
// provider resolution depend on this never failing.
func (s *Store) ProviderMode(ctx context.Context) string {
	values, err := s.raw(ctx)
	if err != nil {
		log.Printf("[settings] read provider_mode: %v", err)
		return ModeAuto
	}
	switch decodeString(values["provider_mode"]) {
	case ModeSidecar, ModeMock:
		return decodeString(values["provider_mode"])
	default:
		return ModeAuto
	}
}

// --------------------------------------------------------------------- view --

// SMTPView is the API projection of the SMTP settings (no password ever).
type SMTPView struct {
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Username    string `json:"username"`
	PasswordSet bool   `json:"password_set"`
	From        string `json:"from"`
	To          string `json:"to"`
}

// View is the GET /api/settings response shape.
type View struct {
	ProviderMode             string   `json:"provider_mode"`
	Cookie                   string   `json:"cookie"`
	DownloadConcurrency      int      `json:"download_concurrency"`
	DownloadQuality          string   `json:"download_quality"`
	ScanPageDelayMs          int      `json:"scan_page_delay_ms"`
	ScanMaxEmptyPages        int      `json:"scan_max_empty_pages"`
	IncrementalStopPages     int      `json:"incremental_stop_pages"`
	CompletenessGapThreshold int      `json:"completeness_gap_threshold"`
	SidecarIdleTimeoutMin    int      `json:"sidecar_idle_timeout_minutes"`
	SMTP                     SMTPView `json:"smtp"`
	NotifyOnNewWork          bool     `json:"notify_on_new_work"`
	NotifyOnFailure          bool     `json:"notify_on_failure"`
}

// maskCookie keeps the first 8 characters and appends "..."; values of 8
// characters or fewer (and empty) are masked completely to avoid a partial leak.
func maskCookie(cookie string) string {
	switch {
	case cookie == "":
		return ""
	case len(cookie) <= 8:
		return "..."
	default:
		return cookie[:8] + "..."
	}
}

// smtpObject decodes the stored "smtp" JSON object (empty map if unset).
func smtpObject(values map[string]string) map[string]any {
	out := map[string]any{}
	if raw, ok := values["smtp"]; ok && raw != "" && raw != "null" {
		_ = json.Unmarshal([]byte(raw), &out)
	}
	return out
}

// View builds the API projection with defaults from the environment.
func (s *Store) View(ctx context.Context) (View, error) {
	values, err := s.raw(ctx)
	if err != nil {
		return View{}, err
	}
	smtp := smtpObject(values)

	v := View{
		ProviderMode:             ModeAuto,
		Cookie:                   maskCookie(decodeString(values["cookie"])),
		DownloadConcurrency:      3,
		DownloadQuality:          "1080p",
		ScanPageDelayMs:          2000,
		ScanMaxEmptyPages:        3,
		IncrementalStopPages:     3,
		CompletenessGapThreshold: 5,
		SidecarIdleTimeoutMin:    int(s.cfg.SidecarIdleTimeout / time.Minute),
		SMTP: SMTPView{
			Host:        asString(smtp["host"]),
			Port:        asInt(smtp["port"]),
			Username:    asString(smtp["username"]),
			PasswordSet: decodeString(values["smtp_password"]) != "",
			From:        asString(smtp["from"]),
			To:          asString(smtp["to"]),
		},
	}
	switch decodeString(values["provider_mode"]) {
	case ModeSidecar, ModeMock:
		v.ProviderMode = decodeString(values["provider_mode"])
	}
	if n := decodeInt(values["download_concurrency"]); n > 0 {
		v.DownloadConcurrency = n
	}
	if q := decodeString(values["download_quality"]); q != "" {
		v.DownloadQuality = q
	}
	if n := decodeInt(values["scan_page_delay_ms"]); n > 0 {
		v.ScanPageDelayMs = n
	}
	if n := decodeInt(values["scan_max_empty_pages"]); n > 0 {
		v.ScanMaxEmptyPages = n
	}
	if n := decodeInt(values["incremental_stop_pages"]); n > 0 {
		v.IncrementalStopPages = n
	}
	if n := decodeInt(values["completeness_gap_threshold"]); n > 0 {
		v.CompletenessGapThreshold = n
	}
	if n := decodeInt(values["sidecar_idle_timeout_minutes"]); n > 0 {
		v.SidecarIdleTimeoutMin = n
	}
	v.NotifyOnNewWork = decodeBool(values["notify_on_new_work"])
	v.NotifyOnFailure = decodeBool(values["notify_on_failure"])
	return v, nil
}

// SMTPSettings returns the full SMTP settings for the notification sender.
func (s *Store) SMTPSettings(ctx context.Context) SMTP {
	values, err := s.raw(ctx)
	if err != nil {
		return SMTP{}
	}
	smtp := smtpObject(values)
	return SMTP{
		Host:     asString(smtp["host"]),
		Port:     asInt(smtp["port"]),
		Username: asString(smtp["username"]),
		Password: decodeString(values["smtp_password"]),
		From:     asString(smtp["from"]),
		To:       asString(smtp["to"]),
	}
}

// -------------------------------------------------------------------- patch --

// Patch is the accepted PATCH /api/settings body (partial update; nil = keep).
type Patch struct {
	ProviderMode             *string    `json:"provider_mode"`
	Cookie                   *string    `json:"cookie"`
	DownloadConcurrency      *int       `json:"download_concurrency"`
	DownloadQuality          *string    `json:"download_quality"`
	ScanPageDelayMs          *int       `json:"scan_page_delay_ms"`
	ScanMaxEmptyPages        *int       `json:"scan_max_empty_pages"`
	IncrementalStopPages     *int       `json:"incremental_stop_pages"`
	CompletenessGapThreshold *int       `json:"completeness_gap_threshold"`
	SidecarIdleTimeoutMin    *int       `json:"sidecar_idle_timeout_minutes"`
	SMTP                     *SMTPPatch `json:"smtp"`
	NotifyOnNewWork          *bool      `json:"notify_on_new_work"`
	NotifyOnFailure          *bool      `json:"notify_on_failure"`
}

// SMTPPatch partially updates the smtp object.
type SMTPPatch struct {
	Host     *string `json:"host"`
	Port     *int    `json:"port"`
	Username *string `json:"username"`
	// Password is write-only: setting it stores it; reading settings never
	// returns it.
	Password *string `json:"password"`
	From     *string `json:"from"`
	To       *string `json:"to"`
}

// Apply validates and persists a patch. On an actual cookie change the cookie
// file is rewritten (hot-reloaded by the sidecar). Returns whether the cookie
// changed.
func (s *Store) Apply(ctx context.Context, patch Patch) (bool, error) {
	updates := make(map[string]string)
	cookieChanged := false

	if patch.ProviderMode != nil {
		switch *patch.ProviderMode {
		case ModeAuto, ModeSidecar, ModeMock:
			updates["provider_mode"] = mustJSON(*patch.ProviderMode)
		default:
			return false, fmt.Errorf("settings: invalid provider_mode %q (auto|sidecar|mock)", *patch.ProviderMode)
		}
	}
	if patch.DownloadConcurrency != nil {
		if *patch.DownloadConcurrency < 1 || *patch.DownloadConcurrency > 8 {
			return false, fmt.Errorf("settings: download_concurrency must be 1-8")
		}
		updates["download_concurrency"] = mustJSON(*patch.DownloadConcurrency)
	}
	if patch.DownloadQuality != nil {
		switch *patch.DownloadQuality {
		case "540p", "720p", "1080p":
			updates["download_quality"] = mustJSON(*patch.DownloadQuality)
		default:
			return false, fmt.Errorf("settings: invalid download_quality %q", *patch.DownloadQuality)
		}
	}
	if patch.ScanPageDelayMs != nil {
		if *patch.ScanPageDelayMs < 1000 || *patch.ScanPageDelayMs > 10000 {
			return false, fmt.Errorf("settings: scan_page_delay_ms must be 1000-10000")
		}
		updates["scan_page_delay_ms"] = mustJSON(*patch.ScanPageDelayMs)
	}
	if patch.ScanMaxEmptyPages != nil {
		if *patch.ScanMaxEmptyPages < 1 || *patch.ScanMaxEmptyPages > 10 {
			return false, fmt.Errorf("settings: scan_max_empty_pages must be 1-10")
		}
		updates["scan_max_empty_pages"] = mustJSON(*patch.ScanMaxEmptyPages)
	}
	if patch.IncrementalStopPages != nil {
		if *patch.IncrementalStopPages < 1 || *patch.IncrementalStopPages > 10 {
			return false, fmt.Errorf("settings: incremental_stop_pages must be 1-10")
		}
		updates["incremental_stop_pages"] = mustJSON(*patch.IncrementalStopPages)
	}
	if patch.CompletenessGapThreshold != nil {
		if *patch.CompletenessGapThreshold < 1 || *patch.CompletenessGapThreshold > 50 {
			return false, fmt.Errorf("settings: completeness_gap_threshold must be 1-50")
		}
		updates["completeness_gap_threshold"] = mustJSON(*patch.CompletenessGapThreshold)
	}
	if patch.SidecarIdleTimeoutMin != nil {
		if *patch.SidecarIdleTimeoutMin < 1 {
			return false, fmt.Errorf("settings: sidecar_idle_timeout_minutes must be >= 1")
		}
		updates["sidecar_idle_timeout_minutes"] = mustJSON(*patch.SidecarIdleTimeoutMin)
	}
	if patch.NotifyOnNewWork != nil {
		updates["notify_on_new_work"] = mustJSON(*patch.NotifyOnNewWork)
	}
	if patch.NotifyOnFailure != nil {
		updates["notify_on_failure"] = mustJSON(*patch.NotifyOnFailure)
	}
	if patch.SMTP != nil {
		p := patch.SMTP
		if p.Port != nil && (*p.Port < 1 || *p.Port > 65535) {
			return false, fmt.Errorf("settings: smtp.port must be 1-65535")
		}
		// Merge into the existing smtp object.
		current := map[string]any{}
		if values, err := s.raw(ctx); err == nil {
			current = smtpObject(values)
		}
		if p.Host != nil {
			current["host"] = *p.Host
		}
		if p.Port != nil {
			current["port"] = *p.Port
		}
		if p.Username != nil {
			current["username"] = *p.Username
		}
		if p.From != nil {
			current["from"] = *p.From
		}
		if p.To != nil {
			current["to"] = *p.To
		}
		updates["smtp"] = mustJSON(current)
		if p.Password != nil {
			// Stored as its own key so View/SMTPSettings treat it separately.
			updates["smtp_password"] = mustJSON(*p.Password)
		}
	}
	if patch.Cookie != nil {
		prev, err := s.raw(ctx)
		if err != nil {
			return false, err
		}
		if decodeString(prev["cookie"]) != *patch.Cookie {
			cookieChanged = true
			updates["cookie"] = mustJSON(*patch.Cookie)
		}
	}

	if len(updates) == 0 {
		return false, nil
	}
	if err := db.WithTx(ctx, s.db, func(tx *sql.Tx) error {
		for k, v := range updates {
			if _, err := tx.Exec(
				`INSERT INTO settings(key, value) VALUES (?, ?)
				 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
				return fmt.Errorf("settings: upsert %s: %w", k, err)
			}
		}
		return nil
	}); err != nil {
		return false, err
	}

	if cookieChanged {
		if err := s.writeCookieFile(*patch.Cookie); err != nil {
			return true, err
		}
	}
	if n := decodeInt(updates["sidecar_idle_timeout_minutes"]); n > 0 && s.onSidecarIdleTimeout != nil {
		s.onSidecarIdleTimeout(time.Duration(n) * time.Minute)
	}
	if n := decodeInt(updates["download_concurrency"]); n > 0 && s.onDownloadConcurrency != nil {
		s.onDownloadConcurrency(n)
	}
	return cookieChanged, nil
}

// writeCookieFile persists the cookie for the Python sidecar.
func (s *Store) writeCookieFile(cookie string) error {
	path := s.CookieFilePath()
	if err := writeFileAtomic(path, []byte(cookie)); err != nil {
		return fmt.Errorf("settings: write cookie file: %w", err)
	}
	log.Printf("[settings] cookie file updated: %s (%d bytes)", path, len(cookie))
	return nil
}

// ------------------------------------------------------------------ helpers --

// writeFileAtomic writes via a temp file + rename so the sidecar never reads
// a half-written cookie file.
func writeFileAtomic(path string, content []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".cookie-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after successful rename
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func mustJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		// Arguments are plain strings/numbers/bools/maps — marshal cannot
		// realistically fail; fall back to an empty JSON string.
		return `""`
	}
	return string(raw)
}

func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	case float64:
		return fmt.Sprintf("%v", t)
	default:
		return ""
	}
}

func asInt(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	default:
		return 0
	}
}

func decodeString(raw string) string {
	if raw == "" {
		return ""
	}
	var out string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return raw // tolerate non-JSON legacy values
	}
	return out
}

func decodeInt(raw string) int {
	if raw == "" {
		return 0
	}
	var out int
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return 0
	}
	return out
}

func decodeBool(raw string) bool {
	if raw == "" {
		return false
	}
	var out bool
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return false
	}
	return out
}
