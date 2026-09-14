// Package spark integrates the douyin-sparkflow "spark" (续火花) engine into
// the archive backend:
//
//   - client.go     typed HTTP client for the stateless Python engine
//   - store.go      SQLite CRUD (Go is the only writer)
//   - sendconfig.go spark_settings.send_config parsing + engine payload mapping
//   - service.go    orchestration shared by the API handlers and scheduler
//   - scheduler.go  30s tick, deterministic due-target selection, serial sends
//
// The engine (spark-engine/, Python + Playwright) keeps no business state:
// every result returned over HTTP is persisted here. All engine endpoints
// require the X-Engine-Token header; DY_SPARK_TOKEN empty disables the whole
// /api/spark/* surface.
package spark

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client is the typed HTTP client for the spark engine.
type Client struct {
	base  string
	token string
	hc    *http.Client
}

// NewClient builds a client. base is the engine root ("http://127.0.0.1:18788").
func NewClient(base, token string) *Client {
	return &Client{
		base:  strings.TrimRight(strings.TrimSpace(base), "/"),
		token: token,
		hc:    &http.Client{Timeout: 30 * time.Second},
	}
}

// BaseURL returns the configured engine root (for the login reverse proxy).
func (c *Client) BaseURL() string { return c.base }

// Token returns the engine token (the login proxy stamps it on proxied
// requests, including WebSocket upgrades).
func (c *Client) Token() string { return c.token }

// Timeout bounds are contract-mandated: regular calls 30s, a send run may
// legitimately take many minutes (one account, up to ~50 targets at 25-70s
// intervals), hence 10 minutes (docs/HUOHUA_EXECUTION_PLAN.md §4).
const (
	defaultTimeout = 30 * time.Second
	sendTimeout    = 10 * time.Minute
)

// APIError is a non-2xx engine reply with its {"detail": ...} body.
type APIError struct {
	Status int
	Detail string
}

func (e *APIError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("spark engine: %s (HTTP %d)", e.Detail, e.Status)
	}
	return fmt.Sprintf("spark engine: HTTP %d", e.Status)
}

// Busy reports whether the engine rejected the call because another task is
// already running (HTTP 409, engine-wide single-task model).
func (e *APIError) Busy() bool { return e.Status == http.StatusConflict }

func (c *Client) do(ctx context.Context, timeout time.Duration, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("spark: encode request: %w", err)
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return fmt.Errorf("spark: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Engine-Token", c.token)

	hc := c.hc
	if timeout != defaultTimeout {
		hc = &http.Client{Timeout: timeout}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("spark: engine unreachable at %s: %w", c.base, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("spark: read engine response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail := ""
		var er struct {
			Detail string `json:"detail"`
		}
		if json.Unmarshal(data, &er) == nil {
			detail = er.Detail
		}
		return &APIError{Status: resp.StatusCode, Detail: detail}
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("spark: decode engine response: %w", err)
		}
	}
	return nil
}

// ---------------------------------------------------------------- payloads --

// EngineHealth is GET /health.
type EngineHealth struct {
	Status      string `json:"status"`
	Version     int    `json:"version"`
	TaskRunning bool   `json:"task_running"`
	TaskKind    string `json:"task_kind"`
}

// Health probes the engine.
func (c *Client) Health(ctx context.Context) (*EngineHealth, error) {
	var out EngineHealth
	if err := c.do(ctx, defaultTimeout, http.MethodGet, "/health", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CookieExport is POST /cookies/export.
type CookieExport struct {
	Cookie      string `json:"cookie"`
	CookieCount int    `json:"cookie_count"`
	ExportedAt  string `json:"exported_at"`
}

// ExportCookies opens the account's browser profile and returns the .douyin.com
// cookie header (never logged, never persisted outside <data_dir>/.cookie).
func (c *Client) ExportCookies(ctx context.Context, profileName string) (*CookieExport, error) {
	var out CookieExport
	err := c.do(ctx, defaultTimeout, http.MethodPost, "/cookies/export",
		map[string]string{"profile_name": profileName}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// FriendPair is one engine-reported friend (normalized key + display name).
type FriendPair struct {
	Key         string `json:"key"`
	DisplayName string `json:"display_name"`
}

// FriendsRefresh is POST /friends/refresh.
type FriendsRefresh struct {
	Friends        []FriendPair `json:"friends"`
	Complete       bool         `json:"complete"`
	ElapsedSeconds float64      `json:"elapsed_seconds"`
}

// RefreshFriends scrapes the creator chat page friend list.
func (c *Client) RefreshFriends(ctx context.Context, profileName string) (*FriendsRefresh, error) {
	var out FriendsRefresh
	err := c.do(ctx, sendTimeout, http.MethodPost, "/friends/refresh",
		map[string]string{"profile_name": profileName}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// SendRunRequest is POST /send/run. Config is the engine-shaped subset of
// send_config (see SendConfig.EnginePayload); PreviousMessages carries the
// last strong message per target so the engine avoids duplicate texts.
type SendRunRequest struct {
	ProfileName      string            `json:"profile_name"`
	UniqueID         string            `json:"unique_id"`
	AccountName      string            `json:"account_name"`
	Targets          []string          `json:"targets"`
	Config           map[string]any    `json:"config"`
	PreviousMessages map[string]string `json:"previous_messages,omitempty"`
}

// SendResult is one target's outcome. The engine only emits the strong and
// failed states (upstream "weak" never leaves its failure queue).
type SendResult struct {
	Target   string `json:"target"`
	Message  string `json:"message"`
	State    string `json:"state"`
	Category string `json:"category"`
	Detail   string `json:"detail"`
	SentAt   string `json:"sent_at"`
}

// SendRunResponse is POST /send/run.
type SendRunResponse struct {
	Results        []SendResult   `json:"results"`
	AccountFailure map[string]any `json:"account_failure"`
	StartedAt      string         `json:"started_at"`
	FinishedAt     string         `json:"finished_at"`
	ElapsedSeconds float64        `json:"elapsed_seconds"`
}

// RunSend executes one account's send loop on the engine (synchronous).
func (c *Client) RunSend(ctx context.Context, req SendRunRequest) (*SendRunResponse, error) {
	var out SendRunResponse
	if err := c.do(ctx, sendTimeout, http.MethodPost, "/send/run", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// LoginStatus is GET /login/status.
type LoginStatus struct {
	Running  bool   `json:"running"`
	LoggedIn bool   `json:"logged_in"`
	UniqueID string `json:"unique_id"`
	Nickname string `json:"nickname"`
}

// LoginStatus probes the login desktop bridge.
func (c *Client) LoginStatus(ctx context.Context) (*LoginStatus, error) {
	var out LoginStatus
	if err := c.do(ctx, defaultTimeout, http.MethodGet, "/login/status", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// LoginExport is POST /login/export: identity + cookie header of the freshly
// logged-in profile.
type LoginExport struct {
	UniqueID    string `json:"unique_id"`
	Nickname    string `json:"nickname"`
	ProfileName string `json:"profile_name"`
	Cookie      string `json:"cookie"`
}

// LoginExport finalizes the login flow (engine copies the login profile into
// uid-<unique_id> and exports its cookies).
func (c *Client) LoginExport(ctx context.Context) (*LoginExport, error) {
	var out LoginExport
	if err := c.do(ctx, defaultTimeout, http.MethodPost, "/login/export", map[string]any{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
