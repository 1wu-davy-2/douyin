package downloader

// Stage 9 acceptance: an image (gallery) work downloads into the aggregated
// title directory as 0001.jpg..000N.jpg plus live0001.mp4, cover.jpg and
// metadata.json; assets carry 4-digit / live+sequence qualities; every CDN
// candidate is tried in order; a re-download replaces the media set wholesale
// instead of stacking rows.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"douyin/backend/internal/provider"
)

// galleryFixture wires a fake CDN with a 3-image gallery work (one live
// segment). The first candidate of image 0001 500s once, forcing the
// downloader onto the second candidate.
func galleryFixture(t *testing.T) (*harness, int64, int64) {
	h := newHarness(t)
	h.start()

	// Extension inference: image 2 hands out a .webp URL.
	img := func(n int, ext string, url string) provider.WorkImage {
		if url == "" {
			url = h.cdn.urlFor("gal_1", fmt.Sprintf("img%d%s", n, ext))
		}
		return provider.WorkImage{URL: url, URLs: []string{url}, Width: 1080, Height: 1440}
	}
	primary := h.cdn.urlFor("gal_1", "img1.jpg")
	alternate := h.cdn.urlFor("gal_1", "img1-alt.jpg")
	h.cdn.failures["/cdn/gal_1/img1.jpg"] = 1 // first candidate fails once

	detail := &provider.WorkDetail{
		ItemID:   "gal_1",
		Title:    "图集/作品: 一?",
		Type:     provider.TypeImage,
		CoverURL: h.cdn.urlFor("gal_1", "cover.jpg"),
		Duration: 0,
		Variants: []provider.Variant{},
		Images: []provider.WorkImage{
			{URL: primary, URLs: []string{primary, alternate}, Width: 1080, Height: 1440},
			img(2, ".webp", ""),
			img(3, ".jpg", ""),
		},
		LiveVideos: []provider.LiveVideo{
			{URL: h.cdn.urlFor("gal_1", "live1.mp4"), URLs: []string{h.cdn.urlFor("gal_1", "live1.mp4")}},
		},
	}
	h.prov.setDetail("gal_1", detail)

	creatorID := h.creator("图集博主", "MS4wLjABAAAAgallery")
	workID := h.imageWork(creatorID, "gal_1", "图集/作品: 一?", nil)
	return h, creatorID, workID
}

func galleryAssets(t *testing.T, h *harness, workID int64) map[string]string {
	t.Helper()
	rows, err := h.db.Query(
		`SELECT kind, COALESCE(quality, ''), path FROM assets WHERE work_id = ? ORDER BY kind, quality`, workID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var kind, quality, path string
		if err := rows.Scan(&kind, &quality, &path); err != nil {
			t.Fatal(err)
		}
		out[kind+"\x00"+quality] = path
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDownloadImageGalleryLayout(t *testing.T) {
	h, _, workID := galleryFixture(t)

	jobID := h.enqueue([]int64{workID}, "1080p").Created[0]
	waitFor(t, 15*time.Second, func() bool { return h.jobState(jobID).Status == StatusSucceeded },
		"gallery job to succeed")

	// Aggregated directory: downloads/{creator}/singles/{safeName(title)}/.
	dir := filepath.Join(h.dataDir, "downloads", "图集博主_MS4wLjABAAAAgallery", "singles", "图集_作品_ 一_")
	for _, name := range []string{"0001.jpg", "0002.webp", "0003.jpg", "live0001.mp4", "cover.jpg", "metadata.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("gallery file %s missing: %v", name, err)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 6 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("gallery dir holds %v, want exactly the 6 deterministic files", names)
	}

	// Asset rows: kind=image with 4-digit sequence quality, live segment as
	// kind=video with "live"+sequence, plus cover/metadata.
	assets := galleryAssets(t, h, workID)
	want := map[string]string{}
	for rel, name := range map[string]string{
		"image\x000001":     "0001.jpg",
		"image\x000002":     "0002.webp",
		"image\x000003":     "0003.jpg",
		"video\x00live0001": "live0001.mp4",
		"cover\x00":         "cover.jpg",
		"metadata\x00":      "metadata.json",
	} {
		want[rel] = filepath.ToSlash(filepath.Join(dir, name))
	}
	for k, wpath := range want {
		got, ok := assets[k]
		if !ok {
			t.Errorf("asset %q missing (have %v)", k, assets)
			continue
		}
		if got != wpath {
			t.Errorf("asset %q path = %q, want %q", k, got, wpath)
		}
	}
	if len(assets) != len(want) {
		t.Fatalf("assets = %v, want exactly %d rows", assets, len(want))
	}

	// Candidate fallback: the failing first candidate was hit exactly once,
	// the download came from the alternate.
	if n := h.cdn.hitCount("/cdn/gal_1/img1.jpg"); n != 1 {
		t.Errorf("first candidate hit %d time(s), want exactly 1 (then fallback)", n)
	}
	if n := h.cdn.hitCount("/cdn/gal_1/img1-alt.jpg"); n != 1 {
		t.Errorf("alternate candidate hit %d time(s), want 1", n)
	}

	// Job bytes accumulate the media sizes (4 media files, cover excluded).
	wantBytes := int64(4) * int64(256<<10)
	st := h.jobState(jobID)
	if st.Total != wantBytes {
		t.Errorf("job total_bytes = %d, want %d", st.Total, wantBytes)
	}

	// metadata.json is the normalized gallery document.
	raw, err := os.ReadFile(filepath.Join(dir, "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("metadata.json: %v", err)
	}
	if meta["type"] != provider.TypeImage || meta["item_id"] != "gal_1" {
		t.Errorf("metadata type/item = %v/%v", meta["type"], meta["item_id"])
	}
	if imgs, ok := meta["images"].([]any); !ok || len(imgs) != 3 {
		t.Errorf("metadata images = %v, want 3 entries", meta["images"])
	}
	// The enqueued quality column is untouched (quality is a video concept).
	if st.Quality != "1080p" {
		t.Errorf("job quality = %q, want the enqueued 1080p", st.Quality)
	}

	// Re-download: same deterministic names (overwrite) and a replaced asset
	// set, not duplicated rows.
	job2 := h.enqueue([]int64{workID}, "720p").Created[0]
	waitFor(t, 15*time.Second, func() bool { return h.jobState(job2).Status == StatusSucceeded },
		"second gallery job to succeed")
	if assets2 := galleryAssets(t, h, workID); len(assets2) != len(want) {
		t.Fatalf("assets after re-download = %d rows, want %d (wholesale replace)", len(assets2), len(want))
	}
	if parts := h.partsUnder(); len(parts) != 0 {
		t.Fatalf(".part files left behind: %v", parts)
	}
}

// A gallery detail without image addresses fails the job with a clear error.
func TestDownloadImageGalleryWithoutImagesFails(t *testing.T) {
	h := newHarness(t)
	h.start()

	h.prov.setDetail("empty_gal", &provider.WorkDetail{
		ItemID:   "empty_gal",
		Title:    "空图集",
		Type:     provider.TypeImage,
		Variants: []provider.Variant{},
	})
	creatorID := h.creator("空图集", "MS4wLjABAAAAemptygal")
	workID := h.imageWork(creatorID, "empty_gal", "空图集", nil)
	jobID := h.enqueue([]int64{workID}, "1080p").Created[0]
	waitFor(t, 10*time.Second, func() bool { return h.jobState(jobID).Status == StatusFailed },
		"gallery job to fail")
	st := h.jobState(jobID)
	if st.Error == nil || !strings.Contains(*st.Error, "no image addresses") {
		t.Fatalf("error = %v, want no-image-addresses cause", st.Error)
	}
}
