// Package provider abstracts the data source for Douyin profiles, post pages
// and work details (docs/api.md "侧车契约").
//
// Two implementations exist:
//   - SidecarProvider: HTTP calls to the Python/F2 sidecar (real mode)
//   - MockProvider: deterministic offline fake data (DY_MOCK=1 / --mock)
//
// Implementations must NEVER swallow a failure into an empty result — that was
// the root cause of missed scans in the legacy system. Every upstream error is
// returned as a non-nil error with context.
package provider

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Provider is the data-source interface used by the (future) scanner.
type Provider interface {
	// Profile fetches a creator profile by sec_uid.
	Profile(ctx context.Context, secUID string) (*Profile, error)
	// PostsPage fetches one page of posts. cursor is the string cursor from
	// the previous page ("" = first page); count is clamped to 1..20.
	PostsPage(ctx context.Context, secUID, cursor string, count int) (*PostsPage, error)
	// WorkDetail fetches the play-address variants of one work.
	WorkDetail(ctx context.Context, itemID string) (*WorkDetail, error)
}

// Profile mirrors the sidecar GET /profile response.
type Profile struct {
	SecUID     string `json:"sec_uid"`
	Nickname   string `json:"nickname"`
	AvatarURL  string `json:"avatar_url"`
	AwemeCount int    `json:"aweme_count"`
	Signature  string `json:"signature"`
}

// PostItem is one entry of the sidecar GET /posts response. duration is in
// milliseconds (sidecar semantics); published_at is RFC3339 UTC or "" when the
// upstream create_time was missing (JSON null).
type PostItem struct {
	ItemID      string `json:"item_id"`
	Title       string `json:"title"`
	CoverURL    string `json:"cover_url"`
	Duration    int    `json:"duration"`
	PublishedAt string `json:"published_at"`
	MixID       string `json:"mix_id"`
	MixName     string `json:"mix_name"`
}

// PostsPage mirrors the sidecar GET /posts response. NextCursor is nil when
// HasMore is false.
type PostsPage struct {
	Items      []PostItem `json:"items"`
	HasMore    bool       `json:"has_more"`
	NextCursor *string    `json:"next_cursor"`
}

// Variant is one quality tier of a work's play addresses.
type Variant struct {
	Quality   string `json:"quality"` // 540p | 720p | 1080p
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	Bitrate   int    `json:"bitrate"`
	SizeBytes int64  `json:"size_bytes"`
	URL       string `json:"url"`
}

// WorkDetail mirrors the sidecar GET /work response.
type WorkDetail struct {
	ItemID   string    `json:"item_id"`
	Title    string    `json:"title"`
	CoverURL string    `json:"cover_url"`
	Duration int       `json:"duration"`
	Variants []Variant `json:"variants"`
}

// ParseCursor converts the API-level string cursor to the integer max_cursor
// the sidecar expects. Empty string means "first page" (0).
func ParseCursor(cursor string) (int64, error) {
	cursor = strings.TrimSpace(cursor)
	if cursor == "" {
		return 0, nil
	}
	v, err := strconv.ParseInt(cursor, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid cursor %q", cursor)
	}
	if v < 0 {
		v = 0
	}
	return v, nil
}

// nextPageCursor renders the next integer cursor as the string form used by
// both the sidecar contract and the mock.
func nextPageCursor(v int64) *string {
	s := strconv.FormatInt(v, 10)
	return &s
}

// clampCount bounds count to the sidecar's rules: <=0 -> default 20, max 20.
func clampCount(count int) int {
	if count <= 0 {
		return 20
	}
	if count > 20 {
		return 20
	}
	return count
}

// requestTimeout is a sane budget for provider calls; the downloader/scanner
// should derive per-call timeouts from their own context anyway. A real
// sidecar /posts call can take up to ~65s (F2-internal retries), hence 90s.
const requestTimeout = 90 * time.Second
