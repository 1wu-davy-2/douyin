package provider

import (
	"context"
	"douyin/backend/internal/mockmedia"
	"errors"
	"fmt"
	"strconv"
	"strings"
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

// Mock gallery dimensions (3:4 portrait), in lockstep with sidecar/main.py.
const (
	mockImageWidth  = 1080
	mockImageHeight = 1440
)

// mockKindFor maps the 0-based global index to (type, image_count): every 7th
// work (index%7==6) is a gallery of 3+index%4 (3..6) images; index%14==13
// additionally carries one live segment (surfaced by WorkDetail only).
// Must match sidecar/main.py mock_kind_for byte for byte.
func mockKindFor(idx int) (string, int) {
	if idx%7 == 6 {
		return TypeImage, 3 + idx%4
	}
	return TypeVideo, 0
}

// mockPosFromItemID parses "mock_0007" into 7; other shapes -> false (treated
// as a video work).
func mockPosFromItemID(itemID string) (int, bool) {
	s := strings.TrimPrefix(itemID, "mock_")
	if s == itemID {
		return 0, false
	}
	pos, err := strconv.Atoi(s)
	if err != nil || pos <= 0 {
		return 0, false
	}
	return pos, true
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
		kind, imageCount := mockKindFor(idx)
		duration := 30 + pos%25
		if kind == TypeImage {
			duration = 0 // contract: image works carry duration 0
		}
		items = append(items, PostItem{
			ItemID:      fmt.Sprintf("mock_%04d", pos),
			Title:       fmt.Sprintf("Mock作品 #%d", pos),
			CoverURL:    fmt.Sprintf("https://mock.example/cover/%d.jpg", pos),
			Duration:    duration,
			PublishedAt: mockPublishedAt(pos),
			MixID:       mixID,
			MixName:     mixName,
			Type:        kind,
			ImageCount:  imageCount,
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
// (http://127.0.0.1:<port>). The handler streams real (truncated) sample MP4
// bytes from internal/mockmedia so mock mode exercises the full
// scan -> download -> SSE -> playable-assets chain offline.
func mockVariants(itemID string) []Variant {
	return []Variant{
		{Quality: "1080p", Width: 1080, Height: 1920, Bitrate: 3000000, SizeBytes: int64(mockmedia.SampleVideoSize),
			URL: fmt.Sprintf("/mockcdn/%s/1080p.mp4", itemID)},
		{Quality: "720p", Width: 720, Height: 1280, Bitrate: 1500000, SizeBytes: int64(mockmedia.Sample720Size),
			URL: fmt.Sprintf("/mockcdn/%s/720p.mp4", itemID)},
		{Quality: "540p", Width: 540, Height: 960, Bitrate: 800000, SizeBytes: int64(mockmedia.Sample540Size),
			URL: fmt.Sprintf("/mockcdn/%s/540p.mp4", itemID)},
	}
}

// WorkDetail returns the deterministic detail for any item_id: video works
// get the 3-tier variant ladder, every 7th work (pos%7==0) is a gallery with
// 3..6 images (/mockcdn/{item}/img{n}.jpg) and every 14th (pos%14==0) also
// carries one live segment (/mockcdn/{item}/live1.mp4).
func (p *MockProvider) WorkDetail(_ context.Context, itemID string) (*WorkDetail, error) {
	if itemID == "" {
		return nil, errors.New("mock provider: item_id is required")
	}
	cover := fmt.Sprintf("/mockcdn/%s/cover.jpg", itemID)
	if pos, ok := mockPosFromItemID(itemID); ok {
		idx := pos - 1
		kind, imageCount := mockKindFor(idx)
		if kind == TypeImage {
			images := make([]WorkImage, 0, imageCount)
			for n := 1; n <= imageCount; n++ {
				u := fmt.Sprintf("/mockcdn/%s/img%d.jpg", itemID, n)
				images = append(images, WorkImage{
					URL:    u,
					URLs:   []string{u},
					Width:  mockImageWidth,
					Height: mockImageHeight,
				})
			}
			live := []LiveVideo{}
			if idx%14 == 13 {
				u := fmt.Sprintf("/mockcdn/%s/live1.mp4", itemID)
				live = append(live, LiveVideo{URL: u, URLs: []string{u}})
			}
			return &WorkDetail{
				ItemID:     itemID,
				Title:      fmt.Sprintf("Mock作品 %s", itemID),
				Type:       TypeImage,
				CoverURL:   cover,
				Duration:   0,
				Variants:   []Variant{},
				Images:     images,
				LiveVideos: live,
			}, nil
		}
	}
	return &WorkDetail{
		ItemID:   itemID,
		Title:    fmt.Sprintf("Mock作品 %s", itemID),
		Type:     TypeVideo,
		CoverURL: cover,
		Duration: 45,
		Variants: mockVariants(itemID),
	}, nil
}
