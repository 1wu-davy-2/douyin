package api

// Downloads + assets endpoint tests: the full mock chain (enqueue -> jobs
// download from this server's own /mockcdn -> summary/list -> assets
// streaming with Range), plus retry/cancel/delete/batch/clear semantics,
// list validation, queue pause/resume and the mock CDN route itself.

import (
	"context"
	"database/sql"
	"douyin/backend/internal/mockmedia"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"douyin/backend/internal/downloader"
	"douyin/backend/internal/events"
)

// sqlDB is a shorthand for the shared test database handle.
type sqlDB = sql.DB

// seedCreator inserts a creator row and returns its id.
func seedCreator(t *testing.T, handle *sqlDB, secUID, nickname string) int64 {
	t.Helper()
	res, err := handle.Exec(
		`INSERT INTO creators (sec_uid, nickname, profile_url, created_at) VALUES (?, ?, ?, ?)`,
		secUID, nickname, "https://www.douyin.com/user/"+secUID, nowRFC3339())
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// seedWork inserts a work row and returns its id.
func seedWork(t *testing.T, handle *sqlDB, creatorID int64, itemID, title string, collectionID any) int64 {
	t.Helper()
	res, err := handle.Exec(`
		INSERT INTO works (creator_id, collection_id, item_id, title, duration, created_at, updated_at)
		VALUES (?, ?, ?, ?, 30, ?, ?)`,
		creatorID, collectionID, itemID, title, nowRFC3339(), nowRFC3339())
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// waitSummary polls GET /api/downloads/summary until cond holds.
func waitSummary(t *testing.T, client *http.Client, server string, cond func(s downloader.Summary) bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(server + "/api/downloads/summary")
		if err == nil {
			var s downloader.Summary
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if json.Unmarshal(raw, &s) == nil && cond(s) {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", msg)
}

func idsString(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = fmt.Sprint(id)
	}
	return strings.Join(parts, ",")
}

// waitForEvent blocks until a download.status event with the given status
// arrives (bounded).
func waitForEvent(t *testing.T, ch <-chan events.Event, status string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case evt := <-ch:
			if ds, ok := evt.Data.(events.DownloadStatus); ok && ds.Status == status {
				return
			}
		case <-deadline:
			t.Fatalf("no download.status %q event within 5s", status)
		}
	}
}

// nthJobID fetches the id of the i-th job in the given status via the list API.
func nthJobID(t *testing.T, client *http.Client, server string, status string, i int) int64 {
	t.Helper()
	resp, err := client.Get(server + "/api/downloads?status=" + status)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var list struct {
		Items []struct {
			ID int64 `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	if i >= len(list.Items) {
		t.Fatalf("not enough %s jobs in list: %s", status, raw)
	}
	return list.Items[i].ID
}

// Full mock-mode chain over the API: enqueue 3 works -> they download from
// the server's own /mockcdn -> summary counts succeed -> list shows them ->
// Range request streams the video -> retry/cancel/delete/batch/clear.
func TestDownloadsAPIFlow(t *testing.T) {
	server, bus, handle := newTestServer(t)
	client := setupAdmin(t, server)

	creatorID := seedCreator(t, handle, "MS4wLjABAAAAdlflow", "流程博主")
	var workIDs []int64
	for i := 1; i <= 3; i++ {
		workIDs = append(workIDs, seedWork(t, handle, creatorID,
			fmt.Sprintf("flow_%d", i), fmt.Sprintf("流程作品 %d", i), nil))
	}

	// POST /api/downloads -> 202 with 3 created.
	status, raw := postJSON(t, client, server.URL+"/api/downloads",
		`{"work_ids":[`+idsString(workIDs)+`],"quality":"1080p"}`)
	if status != http.StatusAccepted {
		t.Fatalf("POST /api/downloads = %d, %s", status, raw)
	}
	var created struct {
		Created []int64 `json:"created"`
		Skipped []struct {
			WorkID int64  `json:"work_id"`
			Reason string `json:"reason"`
		} `json:"skipped"`
	}
	if err := json.Unmarshal(raw, &created); err != nil || len(created.Created) != 3 {
		t.Fatalf("created payload = %s (%v)", raw, err)
	}

	// The three videos (2 MiB each, concurrency 3) finish via the mock CDN.
	waitSummary(t, client, server.URL, func(s downloader.Summary) bool {
		return s.Succeeded == 3 && s.Downloading == 0 && s.Queued == 0
	}, "all three jobs to succeed")

	// Duplicate enqueue of a downloaded quality is skipped.
	status, raw = postJSON(t, client, server.URL+"/api/downloads",
		`{"work_ids":[`+fmt.Sprint(workIDs[0])+`],"quality":"1080p"}`)
	if status != http.StatusAccepted {
		t.Fatalf("re-enqueue = %d, %s", status, raw)
	}
	if err := json.Unmarshal(raw, &created); err != nil || len(created.Created) != 0 ||
		len(created.Skipped) != 1 || !strings.Contains(created.Skipped[0].Reason, "already downloaded") {
		t.Fatalf("re-enqueue payload = %s", raw)
	}

	// List filtered by status, cursor pagination.
	var list struct {
		Items      []map[string]any `json:"items"`
		NextCursor *int64           `json:"next_cursor"`
	}
	resp, err := client.Get(server.URL + "/api/downloads?status=succeeded&limit=2")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("list payload = %s (%v)", raw, err)
	}
	if len(list.Items) != 2 || list.NextCursor == nil {
		t.Fatalf("first page = %d items, next=%v; want 2 + cursor", len(list.Items), list.NextCursor)
	}
	// Capture the first item's id BEFORE the second page fetch reuses the
	// list struct: encoding/json reuses (and overwrites) existing map
	// elements when the backing array has room.
	firstID := int64(list.Items[0]["id"].(float64))
	first := list.Items[0]
	for _, key := range []string{"id", "work_id", "creator_id", "work_title", "creator_nickname",
		"status", "quality", "attempts", "total_bytes", "downloaded_bytes", "speed_bps",
		"error", "queued_at", "started_at", "finished_at"} {
		if _, ok := first[key]; !ok {
			t.Errorf("job view missing field %q", key)
		}
	}
	resp, err = client.Get(fmt.Sprintf("%s/api/downloads?status=succeeded&cursor=%d", server.URL, *list.NextCursor))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	var page2 struct {
		Items      []map[string]any
		NextCursor *int64
	}
	if err := json.Unmarshal(raw, &page2); err != nil || len(page2.Items) != 1 || page2.NextCursor != nil {
		t.Fatalf("second page = %s", raw)
	}
	if int64(page2.Items[0]["id"].(float64)) >= firstID {
		t.Fatalf("second page not older: %v vs %d", page2.Items[0]["id"], firstID)
	}

	// Assets: video first; Range request streams 206 + Content-Range.
	var workAssets []workAsset
	resp, err = client.Get(fmt.Sprintf("%s/api/works/%d/assets", server.URL, workIDs[0]))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if err := json.Unmarshal(raw, &workAssets); err != nil || len(workAssets) != 3 {
		t.Fatalf("assets = %s", raw)
	}
	if workAssets[0].Kind != "video" || workAssets[0].Quality == nil || *workAssets[0].Quality != "1080p" {
		t.Fatalf("first asset = %+v, want 1080p video", workAssets[0])
	}

	req, _ := http.NewRequest(http.MethodGet,
		fmt.Sprintf("%s/api/assets/%d/content", server.URL, workAssets[0].ID), nil)
	req.Header.Set("Range", "bytes=0-1023")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	part, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("range status = %d, want 206", resp.StatusCode)
	}
	if cr := resp.Header.Get("Content-Range"); cr != fmt.Sprintf("bytes 0-1023/%d", mockmedia.SampleVideoSize) {
		t.Fatalf("content-range = %q", cr)
	}
	if len(part) != 1024 {
		t.Fatalf("range body = %d bytes, want 1024", len(part))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "video/mp4" {
		t.Fatalf("content-type = %q, want video/mp4", ct)
	}

	// Full-body GET equals the deterministic mock payload length.
	resp, err = client.Get(fmt.Sprintf("%s/api/assets/%d/content", server.URL, workAssets[0].ID))
	if err != nil {
		t.Fatal(err)
	}
	full, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(full) != mockmedia.SampleVideoSize {
		t.Fatalf("full GET status=%d len=%d", resp.StatusCode, len(full))
	}

	// Retry (quality switch) -> succeeded.
	jobID := firstID
	status, raw = postJSON(t, client, fmt.Sprintf("%s/api/downloads/%d/retry", server.URL, jobID),
		`{"quality":"540p"}`)
	if status != http.StatusOK {
		t.Fatalf("retry = %d, %s", status, raw)
	}
	waitSummary(t, client, server.URL, func(s downloader.Summary) bool {
		return s.Succeeded == 3 && s.Downloading == 0 && s.Queued == 0
	}, "retried job to succeed")

	// Retry again, then observe the queued status event over SSE before
	// canceling it.
	subCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := bus.Subscribe(subCtx)
	status, raw = postJSON(t, client, fmt.Sprintf("%s/api/downloads/%d/retry", server.URL, jobID), `{}`)
	if status != http.StatusOK {
		t.Fatalf("retry-2 = %d, %s", status, raw)
	}
	waitForEvent(t, ch, "queued")

	status, raw = postJSON(t, client, fmt.Sprintf("%s/api/downloads/%d/cancel", server.URL, jobID), `{}`)
	if status != http.StatusOK {
		t.Fatalf("cancel = %d, %s", status, raw)
	}
	waitSummary(t, client, server.URL, func(s downloader.Summary) bool {
		return s.Canceled == 1
	}, "cancel to register")

	// DELETE the canceled job; deleting a second time is 404.
	req, _ = http.NewRequest(http.MethodDelete, fmt.Sprintf("%s/api/downloads/%d", server.URL, jobID), nil)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete = %d, %s", resp.StatusCode, raw)
	}
	req, _ = http.NewRequest(http.MethodDelete, fmt.Sprintf("%s/api/downloads/%d", server.URL, jobID), nil)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("re-delete = %d, want 404", resp.StatusCode)
	}

	// DELETE of a succeeded job is allowed, of a downloading one refused is
	// covered by the downloader tests; here the batch path:
	status, raw = postJSON(t, client, server.URL+"/api/downloads/batch",
		fmt.Sprintf(`{"action":"delete","ids":[%d,%d]}`,
			nthJobID(t, client, server.URL, "succeeded", 0),
			nthJobID(t, client, server.URL, "succeeded", 1)))
	if status != http.StatusOK || !strings.Contains(string(raw), `"affected":2`) {
		t.Fatalf("batch delete = %d, %s", status, raw)
	}
	// Nothing remains (the canceled job was deleted above, the two
	// succeeded ones by the batch), so the sweep is a no-op.
	status, raw = postJSON(t, client, server.URL+"/api/downloads/clear-completed", `{}`)
	if status != http.StatusOK || !strings.Contains(string(raw), `"affected":0`) {
		t.Fatalf("clear-completed = %d, %s", status, raw)
	}
}

// Parameter validation of the list endpoint.
func TestDownloadsListValidation(t *testing.T) {
	server, _, _ := newTestServer(t)
	client := setupAdmin(t, server)

	cases := []struct {
		url    string
		status int
	}{
		{"/api/downloads?status=bogus", http.StatusBadRequest},
		{"/api/downloads?cursor=abc", http.StatusBadRequest},
		{"/api/downloads?cursor=-1", http.StatusBadRequest},
		{"/api/downloads?limit=0", http.StatusBadRequest},
		{"/api/downloads?limit=999", http.StatusBadRequest},
		{"/api/downloads?status=queued,succeeded,failed", http.StatusOK},
		{"/api/downloads", http.StatusOK},
	}
	for _, c := range cases {
		resp, err := client.Get(server.URL + c.url)
		if err != nil {
			t.Fatalf("GET %s: %v", c.url, err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.status {
			t.Errorf("GET %s = %d, want %d", c.url, resp.StatusCode, c.status)
		}
	}
}

// Queue pause/resume through the API; the flag persists in the settings
// table (visible to a fresh Downloader over the same DB).
func TestDownloadsQueuePauseResumeAPI(t *testing.T) {
	server, _, handle := newTestServer(t)
	client := setupAdmin(t, server)

	status, raw := postJSON(t, client, server.URL+"/api/downloads/queue/pause", `{}`)
	if status != http.StatusOK {
		t.Fatalf("pause = %d, %s", status, raw)
	}
	resp, err := client.Get(server.URL + "/api/downloads/summary")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(raw), `"paused":true`) {
		t.Fatalf("summary after pause = %s", raw)
	}

	// The flag is persisted.
	var flag string
	if err := handle.QueryRow(`SELECT value FROM settings WHERE key = 'queue_paused'`).Scan(&flag); err != nil {
		t.Fatalf("paused flag not persisted: %v", err)
	}
	if flag != "true" {
		t.Fatalf("paused flag = %q, want true", flag)
	}

	status, raw = postJSON(t, client, server.URL+"/api/downloads/queue/resume", `{"retry_failed":true}`)
	if status != http.StatusOK {
		t.Fatalf("resume = %d, %s", status, raw)
	}
	resp, err = client.Get(server.URL + "/api/downloads/summary")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(raw), `"paused":false`) {
		t.Fatalf("summary after resume = %s", raw)
	}
}

// Mock CDN route: public, deterministic payload lengths, 404 for unknown
// qualities.
func TestMockCDNRoute(t *testing.T) {
	server, _, _ := newTestServer(t)

	resp, err := http.Get(server.URL + "/mockcdn/mock_0001/720p.mp4")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(body) != mockmedia.Sample720Size {
		t.Fatalf("mock video: status=%d len=%d", resp.StatusCode, len(body))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "video/mp4" {
		t.Fatalf("content-type = %q", ct)
	}

	resp, err = http.Get(server.URL + "/mockcdn/mock_0001/cover.jpg")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(body) == 0 {
		t.Fatalf("mock cover: status=%d len=%d", resp.StatusCode, len(body))
	}

	resp, err = http.Get(server.URL + "/mockcdn/mock_0001/999p.mp4")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown quality = %d, want 404", resp.StatusCode)
	}

	// Stage 9 gallery payloads: img{n}.jpg with a per-index tint and the
	// live segment; img0/imgx (malformed index) stay 404.
	for name, ct := range map[string]string{"img1.jpg": "image/jpeg", "img5.jpg": "image/jpeg", "live1.mp4": "video/mp4"} {
		resp, err := http.Get(server.URL + "/mockcdn/mock_0007/" + name)
		if err != nil {
			t.Fatal(err)
		}
		body, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || len(body) == 0 {
			t.Fatalf("mock gallery %s: status=%d len=%d", name, resp.StatusCode, len(body))
		}
		if got := resp.Header.Get("Content-Type"); got != ct {
			t.Fatalf("mock gallery %s content-type = %q, want %q", name, got, ct)
		}
	}
	resp, err = http.Get(server.URL + "/mockcdn/mock_0007/imgx.jpg")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("malformed gallery name = %d, want 404", resp.StatusCode)
	}
}
