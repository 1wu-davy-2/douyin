package provider

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Mock provider constants — kept in lockstep with sidecar/main.py's --mock
// mode so both produce identical data.
const (
	mockTotal      = 502
	mockAwemeCount = 502
	// mockBaseTS = 2025-01-01T00:00:00Z: publish time of the newest (first) work.
	mockBaseTS = 1735689600
)

// MockProvider returns deterministic fake data derived purely from sec_uid /
// item_id / cursor. It never touches the network, so restarts and processes
// always agree on the data.
type MockProvider struct{}

// NewMockProvider returns the offline provider.
func NewMockProvider() *MockProvider { return &MockProvider{} }

// mockMixFor maps the 1-based global position to its collection: every 10th
// work belongs to "Mock合集A", every 15th to "Mock合集B" (a multiple of both
// falls into A, matching the sidecar's check order).
func mockMixFor(pos int) (mixID, mixName string) {
	if pos%10 == 0 {
		return "mock_mix_1", "Mock合集A"
	}
	if pos%15 == 0 {
		return "mock_mix_2", "Mock合集B"
	}
	return "", ""
}

func mockPublishedAt(pos int) string {
	return time.Unix(mockBaseTS-int64(pos-1)*86400, 0).UTC().Format(time.RFC3339)
}

// Profile always reports the same creator shape, regardless of sec_uid.
func (p *MockProvider) Profile(_ context.Context, secUID string) (*Profile, error) {
	if secUID == "" {
		return nil, errors.New("mock provider: sec_uid is required")
	}
	return &Profile{
		SecUID:     secUID,
		Nickname:   "Mock博主",
		AvatarURL:  "",
		AwemeCount: mockAwemeCount,
		Signature:  "Mock 模式签名",
	}, nil
}

// PostsPage paginates the 502 deterministic works. The cursor is the integer
// offset of the first item to return; with count=20 page 26 (cursor 500)
// holds exactly the last 2 works and has_more=false, next_cursor=null.
func (p *MockProvider) PostsPage(_ context.Context, _ string, cursor string, count int) (*PostsPage, error) {
	offset, err := ParseCursor(cursor)
	if err != nil {
		return nil, fmt.Errorf("mock provider: %w", err)
	}
	n := clampCount(count)

	start := int(offset)
	if start > mockTotal {
		start = mockTotal
	}
	end := start + n
	if end > mockTotal {
		end = mockTotal
	}

	items := make([]PostItem, 0, end-start)
	for idx := start; idx < end; idx++ {
		pos := idx + 1 // 1-based global position
		mixID, mixName := mockMixFor(pos)
		items = append(items, PostItem{
			ItemID:      fmt.Sprintf("mock_%04d", pos),
			Title:       fmt.Sprintf("Mock作品 #%d", pos),
			CoverURL:    fmt.Sprintf("https://mock.example/cover/%d.jpg", pos),
			Duration:    30 + pos%25,
			PublishedAt: mockPublishedAt(pos),
			MixID:       mixID,
			MixName:     mixName,
		})
	}

	hasMore := end < mockTotal
	page := &PostsPage{Items: items, HasMore: hasMore}
	if hasMore {
		page.NextCursor = nextPageCursor(int64(end))
	}
	return page, nil
}

// mockVariants returns the fixed 3-tier quality ladder (descending), with a
// URL per tier. URLs are RELATIVE ("/mockcdn/...") on purpose: the API server
// hosts the fake CDN handler (internal/api/mockcdn.go, registered when
// DY_MOCK=1) and the downloader anchors relative paths at its BaseURL
// (http://127.0.0.1:<port>). The handler streams deterministic pseudo-random
// bytes so mock mode exercises the full scan -> download -> SSE -> assets
// chain offline.
func mockVariants(itemID string) []Variant {
	return []Variant{
		{Quality: "1080p", Width: 1080, Height: 1920, Bitrate: 3000000, SizeBytes: 2097152,
			URL: fmt.Sprintf("/mockcdn/%s/1080p.mp4", itemID)},
		{Quality: "720p", Width: 720, Height: 1280, Bitrate: 1500000, SizeBytes: 1048576,
			URL: fmt.Sprintf("/mockcdn/%s/720p.mp4", itemID)},
		{Quality: "540p", Width: 540, Height: 960, Bitrate: 800000, SizeBytes: 524288,
			URL: fmt.Sprintf("/mockcdn/%s/540p.mp4", itemID)},
	}
}

// WorkDetail returns the deterministic variants for any item_id.
func (p *MockProvider) WorkDetail(_ context.Context, itemID string) (*WorkDetail, error) {
	if itemID == "" {
		return nil, errors.New("mock provider: item_id is required")
	}
	return &WorkDetail{
		ItemID:   itemID,
		Title:    fmt.Sprintf("Mock作品 %s", itemID),
		CoverURL: fmt.Sprintf("/mockcdn/%s/cover.jpg", itemID),
		Duration: 45,
		Variants: mockVariants(itemID),
	}, nil
}
