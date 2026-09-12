package provider

import (
	"context"
	"douyin/backend/internal/mockmedia"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// The full mock pagination walk must yield 26 pages: 25 full pages of 20 plus
// a final page with exactly 2 items and has_more=false / next_cursor=null.
func TestMockPagination502Works(t *testing.T) {
	mock := NewMockProvider()
	ctx := context.Background()

	pages := 0
	total := 0
	cursor := ""
	seen := map[string]bool{}
	for {
		page, err := mock.PostsPage(ctx, "sec_demo", cursor, 20)
		if err != nil {
			t.Fatalf("page %d: %v", pages+1, err)
		}
		pages++
		total += len(page.Items)
		for _, item := range page.Items {
			if seen[item.ItemID] {
				t.Fatalf("duplicate item %s across pages", item.ItemID)
			}
			seen[item.ItemID] = true
		}
		if !page.HasMore {
			if page.NextCursor != nil {
				t.Fatalf("has_more=false but next_cursor=%v (must be null)", *page.NextCursor)
			}
			break
		}
		if page.NextCursor == nil {
			t.Fatal("has_more=true but next_cursor is null")
		}
		if pages > 30 {
			t.Fatal("pagination does not terminate")
		}
		cursor = *page.NextCursor
	}

	if pages != 26 {
		t.Fatalf("pages = %d, want 26", pages)
	}
	if total != 502 {
		t.Fatalf("total items = %d, want 502", total)
	}
	if len(seen) != 502 {
		t.Fatalf("unique items = %d, want 502", len(seen))
	}
}

// Page 26 is reached directly at cursor 500 and holds exactly 2 items.
func TestMockLastPage(t *testing.T) {
	mock := NewMockProvider()
	ctx := context.Background()

	page, err := mock.PostsPage(ctx, "sec_demo", "500", 20)
	if err != nil {
		t.Fatalf("cursor 500: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("cursor 500 items = %d, want 2", len(page.Items))
	}
	if page.HasMore || page.NextCursor != nil {
		t.Fatalf("cursor 500: has_more=%v next_cursor=%v, want false/null", page.HasMore, page.NextCursor)
	}
	if page.Items[0].ItemID != "mock_0501" || page.Items[1].ItemID != "mock_0502" {
		t.Fatalf("cursor 500 items = %s, %s; want mock_0501, mock_0502",
			page.Items[0].ItemID, page.Items[1].ItemID)
	}

	// Cursor beyond the total yields an empty (but well-formed) page.
	page, err = mock.PostsPage(ctx, "sec_demo", "502", 20)
	if err != nil {
		t.Fatalf("cursor 502: %v", err)
	}
	if len(page.Items) != 0 || page.HasMore || page.NextCursor != nil {
		t.Fatalf("cursor 502 past end: %+v", page)
	}
}

// Profile shape matches the sidecar --mock behavior.
func TestMockProfile(t *testing.T) {
	mock := NewMockProvider()
	profile, err := mock.Profile(context.Background(), "MS4wLjABAAAA_demo")
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	if profile.SecUID != "MS4wLjABAAAA_demo" {
		t.Fatalf("sec_uid = %q", profile.SecUID)
	}
	if profile.Nickname != "Mock博主" {
		t.Fatalf("nickname = %q, want Mock博主", profile.Nickname)
	}
	if profile.AwemeCount != 502 {
		t.Fatalf("aweme_count = %d, want 502", profile.AwemeCount)
	}
}

// Every 10th work is in Mock合集A, every 15th in Mock合集B; multiples of both
// fall into A (sidecar check order).
func TestMockCollectionAssignment(t *testing.T) {
	mock := NewMockProvider()
	ctx := context.Background()

	// Grab the first 20 items of page 1 (positions 1..20).
	page, err := mock.PostsPage(ctx, "sec_demo", "", 20)
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	expect := map[int][2]string{
		10: {"mock_mix_1", "Mock合集A"},
		15: {"mock_mix_2", "Mock合集B"},
		20: {"mock_mix_1", "Mock合集A"},
	}
	for _, item := range page.Items {
		pos, err := strconv.Atoi(item.ItemID[len("mock_"):])
		if err != nil {
			t.Fatalf("parse pos from %s: %v", item.ItemID, err)
		}
		want, ok := expect[pos]
		if ok {
			if item.MixID != want[0] || item.MixName != want[1] {
				t.Fatalf("pos %d: mix=(%q,%q), want (%q,%q)", pos, item.MixID, item.MixName, want[0], want[1])
			}
		} else if item.MixID != "" || item.MixName != "" {
			t.Fatalf("pos %d: mix=(%q,%q), want empty", pos, item.MixID, item.MixName)
		}
	}
}

// WorkDetail returns the deterministic 3-tier variants for a video work
// (mock_0001; mock_0007 is a stage-9 gallery, see TestMockImagePostRules).
func TestMockWorkDetail(t *testing.T) {
	mock := NewMockProvider()
	detail, err := mock.WorkDetail(context.Background(), "mock_0001")
	if err != nil {
		t.Fatalf("work detail: %v", err)
	}
	if detail.ItemID != "mock_0001" || detail.Duration != 45 || detail.Type != TypeVideo {
		t.Fatalf("detail = %+v", detail)
	}
	if len(detail.Images) != 0 || len(detail.LiveVideos) != 0 {
		t.Fatalf("video work carries images/live: %+v", detail)
	}
	if len(detail.Variants) != 3 {
		t.Fatalf("variants = %d, want 3", len(detail.Variants))
	}
	qualities := map[string]Variant{}
	for _, v := range detail.Variants {
		qualities[v.Quality] = v
		// Mock URLs are relative /mockcdn/... paths hosted by the API server
		// (the downloader anchors them at its BaseURL).
		if !strings.HasPrefix(v.URL, "/mockcdn/mock_0001/") || !strings.HasSuffix(v.URL, ".mp4") {
			t.Fatalf("quality %s: unexpected mock url %q", v.Quality, v.URL)
		}
	}
	if !strings.HasPrefix(detail.CoverURL, "/mockcdn/mock_0001/cover.jpg") {
		t.Fatalf("unexpected mock cover url %q", detail.CoverURL)
	}
	for q, want := range map[string][4]int{
		"1080p": {1080, 1920, 3000000, mockmedia.SampleVideoSize},
		"720p":  {720, 1280, 1500000, mockmedia.Sample720Size},
		"540p":  {540, 960, 800000, mockmedia.Sample540Size},
	} {
		v, ok := qualities[q]
		if !ok {
			t.Fatalf("missing quality tier %s", q)
		}
		if v.Width != want[0] || v.Height != want[1] || v.Bitrate != want[2] || v.SizeBytes != int64(want[3]) {
			t.Fatalf("quality %s: got %+v", q, v)
		}
	}
}

// Determinism: same sec_uid/item_id -> identical data across calls.
func TestMockDeterministic(t *testing.T) {
	mock := NewMockProvider()
	ctx := context.Background()

	a, _ := mock.PostsPage(ctx, "secX", "40", 20)
	b, _ := mock.PostsPage(ctx, "secX", "40", 20)
	c, _ := mock.PostsPage(ctx, "secY", "40", 20)
	if len(a.Items) != len(b.Items) || len(a.Items) != len(c.Items) {
		t.Fatal("page size differs between calls")
	}
	for i := range a.Items {
		if a.Items[i] != b.Items[i] || a.Items[i].ItemID != c.Items[i].ItemID {
			t.Fatalf("item %d differs between calls/sec_uids: %+v %+v %+v", i, a.Items[i], b.Items[i], c.Items[i])
		}
	}
	if a.Items[0].PublishedAt != "2024-11-22T00:00:00Z" { // pos 41: base - 40 days
		t.Fatalf("published_at[0] = %s", a.Items[0].PublishedAt)
	}

	d1, _ := mock.WorkDetail(ctx, "mock_0003")
	d2, _ := mock.WorkDetail(ctx, "mock_0003")
	if d1.Title != d2.Title || len(d1.Variants) != len(d2.Variants) {
		t.Fatal("work detail not deterministic")
	}
}

// published_at must be RFC3339 UTC.
func TestMockPublishedAtFormat(t *testing.T) {
	mock := NewMockProvider()
	page, err := mock.PostsPage(context.Background(), "secX", "", 20)
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	if page.Items[0].PublishedAt != "2025-01-01T00:00:00Z" {
		t.Fatalf("first published_at = %s, want 2025-01-01T00:00:00Z", page.Items[0].PublishedAt)
	}
}

// Stage 9 mock gallery rules: every 7th work (index%7==6, i.e. pos%7==0) is
// an image post with 3+index%4 (3..6) images and duration 0; every 14th
// (pos%14==0) additionally carries one live segment in WorkDetail. Must stay
// byte-for-byte aligned with sidecar/main.py mock_kind_for.
func TestMockImagePostRules(t *testing.T) {
	mock := NewMockProvider()
	ctx := context.Background()

	// Walk all 502 works and count the image posts.
	imageCount := 0
	for cursor := ""; ; {
		page, err := mock.PostsPage(ctx, "sec_demo", cursor, 20)
		if err != nil {
			t.Fatalf("posts page: %v", err)
		}
		for _, item := range page.Items {
			pos, err := strconv.Atoi(strings.TrimPrefix(item.ItemID, "mock_"))
			if err != nil {
				t.Fatalf("parse %s: %v", item.ItemID, err)
			}
			idx := pos - 1
			if idx%7 == 6 {
				imageCount++
				if item.Type != TypeImage {
					t.Fatalf("pos %d: type = %q, want image", pos, item.Type)
				}
				if want := 3 + idx%4; item.ImageCount != want {
					t.Fatalf("pos %d: image_count = %d, want %d", pos, item.ImageCount, want)
				}
				if item.Duration != 0 {
					t.Fatalf("pos %d: image post duration = %d, want 0", pos, item.Duration)
				}
			} else {
				if item.Type != TypeVideo || item.ImageCount != 0 {
					t.Fatalf("pos %d: type=%q count=%d, want video/0", pos, item.Type, item.ImageCount)
				}
				if item.Duration <= 0 {
					t.Fatalf("pos %d: video duration = %d, want > 0", pos, item.Duration)
				}
			}
		}
		if !page.HasMore {
			break
		}
		cursor = *page.NextCursor
	}
	// 502 works, floor(502/7)=71 galleries; 36 of them (pos%14==0, even idx)
	// carry a live segment: floor((502+7)/14)=36... exactly idx%14==13 ->
	// pos in {14,28,...}.
	if imageCount != 71 {
		t.Fatalf("image posts = %d, want 71", imageCount)
	}

	// Detail of the first gallery (mock_0007: idx 6 -> 3+6%%4=5 images, no
	// live).
	d, err := mock.WorkDetail(ctx, "mock_0007")
	if err != nil {
		t.Fatalf("work detail: %v", err)
	}
	if d.Type != TypeImage || d.Duration != 0 || len(d.Variants) != 0 {
		t.Fatalf("detail type/duration/variants = %q/%d/%d, want image/0/[]", d.Type, d.Duration, len(d.Variants))
	}
	if len(d.Images) != 5 {
		t.Fatalf("images = %d, want 5", len(d.Images))
	}
	for i, img := range d.Images {
		want := fmt.Sprintf("/mockcdn/mock_0007/img%d.jpg", i+1)
		if img.URL != want {
			t.Fatalf("image %d url = %q, want %q", i+1, img.URL, want)
		}
		if len(img.Candidates()) != 1 || img.Candidates()[0] != want {
			t.Fatalf("image %d candidates = %v, want [%s]", i+1, img.Candidates(), want)
		}
		if img.Width != mockImageWidth || img.Height != mockImageHeight {
			t.Fatalf("image %d size = %dx%d", i+1, img.Width, img.Height)
		}
	}
	if len(d.LiveVideos) != 0 {
		t.Fatalf("mock_0007 live videos = %d, want 0", len(d.LiveVideos))
	}

	// mock_0014 (idx 13) carries exactly one live segment.
	d, err = mock.WorkDetail(ctx, "mock_0014")
	if err != nil {
		t.Fatalf("work detail 14: %v", err)
	}
	if d.Type != TypeImage || len(d.Images) != 3+13%4 { // idx 13 -> 3+1=4
		t.Fatalf("mock_0014: type=%q images=%d", d.Type, len(d.Images))
	}
	if len(d.LiveVideos) != 1 {
		t.Fatalf("mock_0014 live videos = %d, want 1", len(d.LiveVideos))
	}
	if want := "/mockcdn/mock_0014/live1.mp4"; d.LiveVideos[0].URL != want ||
		len(d.LiveVideos[0].Candidates()) != 1 || d.LiveVideos[0].Candidates()[0] != want {
		t.Fatalf("live url = %v, want [%s]", d.LiveVideos[0].Candidates(), want)
	}

	// /posts and /work must agree on the classification.
	page, err := mock.PostsPage(ctx, "sec_demo", "0", 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range page.Items {
		d, err := mock.WorkDetail(ctx, item.ItemID)
		if err != nil {
			t.Fatalf("detail %s: %v", item.ItemID, err)
		}
		if d.Type != item.Type {
			t.Fatalf("%s: /posts type=%q but /work type=%q", item.ItemID, item.Type, d.Type)
		}
		if item.Type == TypeVideo && len(d.Variants) == 0 {
			t.Fatalf("%s: video work without variants", item.ItemID)
		}
		if item.Type == TypeImage && len(d.Images) != item.ImageCount {
			t.Fatalf("%s: /work images=%d but /posts image_count=%d", item.ItemID, len(d.Images), item.ImageCount)
		}
	}
}
