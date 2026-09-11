package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"douyin/backend/internal/sidecar"
)

// SidecarProvider calls the Python sidecar over HTTP. Every non-200 response
// is converted into an error carrying the sidecar's {"error": ...} message —
// a failed upstream call never turns into an empty page (the legacy miss-scan
// bug).
type SidecarProvider struct {
	endpoint sidecar.Endpoint
	client   *http.Client // 90s: F2-internal retries can take up to ~65s
}

// NewSidecarProvider wraps a sidecar Endpoint (the Manager).
func NewSidecarProvider(endpoint sidecar.Endpoint) *SidecarProvider {
	return &SidecarProvider{
		endpoint: endpoint,
		client:   &http.Client{Timeout: requestTimeout},
	}
}

// get performs one sidecar GET: ensure the process is up, call with the
// shared token, decode into out, and turn any non-200 into a rich error.
func (p *SidecarProvider) get(ctx context.Context, path string, query url.Values, out any) error {
	p.endpoint.Touch()
	if err := p.endpoint.Ensure(ctx); err != nil {
		return fmt.Errorf("sidecar %s: %w", path, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		p.endpoint.BaseURL()+path+"?"+query.Encode(), nil)
	if err != nil {
		return fmt.Errorf("sidecar %s: build request: %w", path, err)
	}
	req.Header.Set("X-Sidecar-Token", p.endpoint.Token())

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("sidecar %s: %w", path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("sidecar %s: read response: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return sidecarError(path, resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("sidecar %s: decode response: %w", path, err)
	}
	p.endpoint.Touch()
	return nil
}

// sidecarError extracts {"error": ...} from a failed sidecar response; if the
// body is not that shape, the raw body is included instead. Never silent.
func sidecarError(path string, status int, body []byte) error {
	var payload struct {
		Error string `json:"error"`
	}
	if len(body) > 0 && json.Unmarshal(body, &payload) == nil && payload.Error != "" {
		return fmt.Errorf("sidecar %s: HTTP %d: %s", path, status, payload.Error)
	}
	return fmt.Errorf("sidecar %s: HTTP %d: %s", path, status, truncate(string(body), 200))
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// Profile implements Provider.
func (p *SidecarProvider) Profile(ctx context.Context, secUID string) (*Profile, error) {
	if strings.TrimSpace(secUID) == "" {
		return nil, fmt.Errorf("sidecar /profile: sec_uid is required")
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, requestTimeout)
		defer cancel()
	}
	var out Profile
	query := url.Values{"sec_uid": {secUID}}
	if err := p.get(ctx, "/profile", query, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PostsPage implements Provider.
func (p *SidecarProvider) PostsPage(ctx context.Context, secUID, cursor string, count int) (*PostsPage, error) {
	cursorInt, err := ParseCursor(cursor)
	if err != nil {
		return nil, fmt.Errorf("sidecar /posts: %w", err)
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, requestTimeout)
		defer cancel()
	}
	var out PostsPage
	query := url.Values{
		"sec_uid": {secUID},
		"cursor":  {fmt.Sprintf("%d", cursorInt)},
		"count":   {fmt.Sprintf("%d", clampCount(count))},
	}
	if err := p.get(ctx, "/posts", query, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// WorkDetail implements Provider.
func (p *SidecarProvider) WorkDetail(ctx context.Context, itemID string) (*WorkDetail, error) {
	if strings.TrimSpace(itemID) == "" {
		return nil, fmt.Errorf("sidecar /work: item_id is required")
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, requestTimeout)
		defer cancel()
	}
	var out WorkDetail
	query := url.Values{"item_id": {itemID}}
	if err := p.get(ctx, "/work", query, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
