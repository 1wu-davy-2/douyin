package api

// Stage 9 acceptance for the works/assets read surface: works list and detail
// carry type + image_count, and asset lists follow the v1.1 ordering
// (video newest-first -> image quality ascending -> cover -> metadata).

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// seedGalleryWorks inserts one video work (two quality tiers, newest = 1080p)
// and one gallery work (three images, one live clip) under a fresh creator.
func seedGalleryWorks(t *testing.T, h *sql.DB) (creatorID, videoWork, galleryWork int64) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := h.Exec(
		`INSERT INTO creators (sec_uid, nickname, profile_url, created_at)
		 VALUES ('MS4wLjABAAAAapi_gal', '图集API', 'u', ?)`, now)
	if err != nil {
		t.Fatal(err)
	}
	if creatorID, err = res.LastInsertId(); err != nil {
		t.Fatal(err)
	}

	res, err = h.Exec(
		`INSERT INTO works (creator_id, item_id, title, duration, created_at, updated_at)
		 VALUES (?, 'vid_api', '视频作品', 30, ?, ?)`, creatorID, now, now)
	if err != nil {
		t.Fatal(err)
	}
	if videoWork, err = res.LastInsertId(); err != nil {
		t.Fatal(err)
	}

	res, err = h.Exec(
		`INSERT INTO works (creator_id, item_id, title, type, duration, created_at, updated_at)
		 VALUES (?, 'gal_api', '图集作品', 'image', 0, ?, ?)`, creatorID, now, now)
	if err != nil {
		t.Fatal(err)
	}
	if galleryWork, err = res.LastInsertId(); err != nil {
		t.Fatal(err)
	}

	// Video work: 720p inserted first, 1080p second (newest id wins the
	// "newest first" ordering).
	for _, a := range []struct{ quality, path string }{
		{"720p", "downloads/c/singles/v/720p.mp4"},
		{"1080p", "downloads/c/singles/v/1080p.mp4"},
	} {
		if _, err := h.Exec(
			`INSERT INTO assets (work_id, kind, path, size_bytes, quality, created_at)
			 VALUES (?, 'video', ?, 10, ?, ?)`, videoWork, a.path, a.quality, now); err != nil {
			t.Fatal(err)
		}
	}
	// Gallery work: cover/metadata inserted BEFORE the media on purpose so
	// the ordering cannot depend on ids.
	for _, a := range []struct{ kind, quality, path string }{
		{"cover", "", "downloads/c/singles/g/cover.jpg"},
		{"metadata", "", "downloads/c/singles/g/metadata.json"},
		{"image", "0003", "downloads/c/singles/g/0003.jpg"},
		{"video", "live0001", "downloads/c/singles/g/live0001.mp4"},
		{"image", "0001", "downloads/c/singles/g/0001.jpg"},
		{"image", "0002", "downloads/c/singles/g/0002.jpg"},
	} {
		if _, err := h.Exec(
			`INSERT INTO assets (work_id, kind, path, size_bytes, quality, created_at)
			 VALUES (?, ?, ?, 10, ?, ?)`, galleryWork, a.kind, a.path, nullIfEmpty(a.quality), now); err != nil {
			t.Fatal(err)
		}
	}
	return creatorID, videoWork, galleryWork
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func TestWorksListCarriesTypeAndImageCount(t *testing.T) {
	server, _, database := newTestServer(t)
	client := setupAdmin(t, server)
	creatorID, videoWork, galleryWork := seedGalleryWorks(t, database)

	status, raw := getJSON(t, client, fmt.Sprintf("%s/api/creators/%d/works", server.URL, creatorID))
	if status != http.StatusOK {
		t.Fatalf("works list = %d %s", status, raw)
	}
	var page struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(page.Items))
	}
	byID := map[int64]map[string]any{}
	for _, it := range page.Items {
		byID[int64(it["id"].(float64))] = it
	}
	v := byID[videoWork]
	if v["type"] != "video" || v["image_count"].(float64) != 0 {
		t.Fatalf("video work type/image_count = %v/%v", v["type"], v["image_count"])
	}
	g := byID[galleryWork]
	if g["type"] != "image" || g["image_count"].(float64) != 3 {
		t.Fatalf("gallery work type/image_count = %v/%v", g["type"], g["image_count"])
	}
	if g["duration"].(float64) != 0 {
		t.Fatalf("gallery duration = %v, want 0", g["duration"])
	}
}

func TestWorkDetailCarriesTypeAndImageCount(t *testing.T) {
	server, _, database := newTestServer(t)
	client := setupAdmin(t, server)
	_, _, galleryWork := seedGalleryWorks(t, database)

	status, raw := getJSON(t, client, fmt.Sprintf("%s/api/works/%d", server.URL, galleryWork))
	if status != http.StatusOK {
		t.Fatalf("work detail = %d %s", status, raw)
	}
	var detail map[string]any
	if err := json.Unmarshal(raw, &detail); err != nil {
		t.Fatal(err)
	}
	if detail["type"] != "image" || detail["image_count"].(float64) != 3 {
		t.Fatalf("detail type/image_count = %v/%v", detail["type"], detail["image_count"])
	}

	// Assets in the contract order: live clip (video kind) -> images by
	// sequence -> cover -> metadata, regardless of insert order/ids.
	assets := detail["assets"].([]any)
	want := []string{"live0001.mp4", "0001.jpg", "0002.jpg", "0003.jpg", "cover.jpg", "metadata.json"}
	if len(assets) != len(want) {
		t.Fatalf("assets = %d, want %d (%v)", len(assets), len(want), assets)
	}
	for i, w := range want {
		a := assets[i].(map[string]any)
		path := a["path"].(string)
		if got := path[len(path)-len(w):]; got != w {
			t.Fatalf("asset %d = %s, want suffix %s", i, path, w)
		}
	}
}

func TestWorkAssetsEndpointOrdering(t *testing.T) {
	server, _, database := newTestServer(t)
	client := setupAdmin(t, server)
	_, videoWork, _ := seedGalleryWorks(t, database)

	// Video work: newest video asset (1080p, higher id) first.
	status, raw := getJSON(t, client, fmt.Sprintf("%s/api/works/%d/assets", server.URL, videoWork))
	if status != http.StatusOK {
		t.Fatalf("assets = %d %s", status, raw)
	}
	var assets []map[string]any
	if err := json.Unmarshal(raw, &assets); err != nil {
		t.Fatal(err)
	}
	if len(assets) != 2 {
		t.Fatalf("assets = %d, want 2", len(assets))
	}
	if assets[0]["quality"] != "1080p" || assets[1]["quality"] != "720p" {
		t.Fatalf("video asset order = %v/%v, want 1080p first", assets[0]["quality"], assets[1]["quality"])
	}
}
