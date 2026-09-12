package api

// End-to-end test of the creators/works/subscriptions surface in mock mode
// (acceptance for stage 4): POST /api/creators -> 202 -> the asynchronous scan
// runs -> works=502, collections=2, >=26 scan.progress events on SSE,
// batch-ids=502, subscription CRUD round-trip.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func postJSON(t *testing.T, client *http.Client, url, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func patchJSON(t *testing.T, client *http.Client, url, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPatch, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func getJSON(t *testing.T, client *http.Client, url string) (int, []byte) {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// drainSSE consumes the event stream in the background and reports the raw
// bytes when stopped.
func drainSSE(t *testing.T, client *http.Client, url string) (func() string, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	var mu sync.Mutex
	var buf strings.Builder
	done := make(chan struct{})
	go func() {
		defer close(done)
		reader := resp.Body
		chunk := make([]byte, 4096)
		for {
			n, err := reader.Read(chunk)
			if n > 0 {
				mu.Lock()
				buf.Write(chunk[:n])
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	return func() string {
			mu.Lock()
			defer mu.Unlock()
			return buf.String()
		}, func() {
			cancel()
			<-done
			resp.Body.Close()
		}
}

// waitScanFinished polls the creator detail until the last scan reaches a
// terminal status (or fails after the deadline).
func waitScanFinished(t *testing.T, client *http.Client, serverURL string, creatorID int64, timeout time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last map[string]any
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		status, raw := getJSON(t, client, fmt.Sprintf("%s/api/creators/%d", serverURL, creatorID))
		if status != http.StatusOK {
			t.Fatalf("GET creator: %d %s", status, raw)
		}
		var detail map[string]any
		if err := json.Unmarshal(raw, &detail); err != nil {
			t.Fatalf("decode creator detail: %v (%s)", err, raw)
		}
		last = detail
		ls, _ := detail["last_scan"].(map[string]any)
		if ls == nil {
			continue
		}
		switch ls["status"] {
		case "succeeded", "partial", "failed":
			return detail
		}
	}
	t.Fatalf("scan did not finish in %s: %v", timeout, last)
	return nil
}

func TestCreatorScanE2EMock(t *testing.T) {
	server, _, _ := newTestServer(t)
	client := setupAdmin(t, server)

	// SSE stream subscribed before the scan starts: must see the page events.
	stream, stopStream := drainSSE(t, client, server.URL+"/api/events")
	defer stopStream()

	// 1. POST /api/creators with a profile URL -> 202 {creator_id, scan_id}.
	status, raw := postJSON(t, client, server.URL+"/api/creators",
		`{"profile_url":"https://www.douyin.com/user/MS4wLjABAAAA_e2e_mock?sec=1&utm=x"}`)
	if status != http.StatusAccepted {
		t.Fatalf("POST /api/creators = %d %s, want 202", status, raw)
	}
	var created struct {
		CreatorID int64 `json:"creator_id"`
		ScanID    int64 `json:"scan_id"`
	}
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatalf("decode 202 body: %v (%s)", err, raw)
	}
	if created.CreatorID <= 0 || created.ScanID <= 0 {
		t.Fatalf("202 body %s lacks ids", raw)
	}

	// 2. Wait for the scan to finish.
	detail := waitScanFinished(t, client, server.URL, created.CreatorID, 30*time.Second)
	ls := detail["last_scan"].(map[string]any)
	if ls["status"] != "succeeded" {
		t.Fatalf("scan status = %v (err %v), want succeeded", ls["status"], ls["last_error"])
	}
	if ls["pages"].(float64) != 26 {
		t.Fatalf("pages = %v, want 26", ls["pages"])
	}
	if ls["new_count"].(float64) != 502 {
		t.Fatalf("new_count = %v, want 502", ls["new_count"])
	}
	if detail["works_count"].(float64) != 502 {
		t.Fatalf("works_count = %v, want 502", detail["works_count"])
	}
	if detail["nickname"] != "Mock博主" {
		t.Fatalf("nickname = %v, want the mock profile refresh", detail["nickname"])
	}
	if detail["reported_work_count"].(float64) != 502 {
		t.Fatalf("reported_work_count = %v", detail["reported_work_count"])
	}

	// 3. SSE: at least 26 scan.progress events + one scan.done.
	streamText := stream()
	if got := strings.Count(streamText, "event: scan.progress"); got < 26 {
		t.Fatalf("scan.progress events = %d, want >= 26", got)
	}
	if !strings.Contains(streamText, "event: scan.done") ||
		!strings.Contains(streamText, `"status":"succeeded"`) {
		t.Fatalf("stream missing scan.done succeeded:\n%.500s", streamText)
	}

	// 4. Works list: server-side pagination, page 26 holds the last 2.
	status, raw = getJSON(t, client,
		fmt.Sprintf("%s/api/creators/%d/works?page=26&page_size=20", server.URL, created.CreatorID))
	if status != http.StatusOK {
		t.Fatalf("GET works p26 = %d %s", status, raw)
	}
	var page struct {
		Items []map[string]any `json:"items"`
		Total float64          `json:"total"`
		Page  float64          `json:"page"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 502 || len(page.Items) != 2 || page.Page != 26 {
		t.Fatalf("page 26 = total %v items %d page %v, want 502/2/26", page.Total, len(page.Items), page.Page)
	}
	first := page.Items[0]
	for _, field := range []string{"id", "item_id", "title", "cover_url", "duration", "published_at",
		"collection_id", "collection_name", "dl_status", "downloaded_quality", "created_at"} {
		if _, ok := first[field]; !ok {
			t.Fatalf("work item missing field %q: %v", field, first)
		}
	}
	if first["dl_status"] != "none" {
		t.Fatalf("dl_status = %v, want none (no jobs)", first["dl_status"])
	}

	// q filter + sort validation.
	status, raw = getJSON(t, client,
		fmt.Sprintf("%s/api/creators/%d/works?q=%%23499", server.URL, created.CreatorID))
	var qpage struct {
		Total float64 `json:"total"`
	}
	json.Unmarshal(raw, &qpage)
	if status != http.StatusOK || qpage.Total != 1 {
		t.Fatalf("q filter = %d total %v, want 200/1", status, qpage.Total)
	}
	status, _ = getJSON(t, client,
		fmt.Sprintf("%s/api/creators/%d/works?sort=bogus", server.URL, created.CreatorID))
	if status != http.StatusBadRequest {
		t.Fatalf("bogus sort = %d, want 400", status)
	}

	// 5. Collections: every 10th work in A, every 15th in B.
	status, raw = getJSON(t, client,
		fmt.Sprintf("%s/api/creators/%d/collections", server.URL, created.CreatorID))
	if status != http.StatusOK {
		t.Fatalf("GET collections = %d %s", status, raw)
	}
	var colls []map[string]any
	if err := json.Unmarshal(raw, &colls); err != nil {
		t.Fatal(err)
	}
	if len(colls) != 2 {
		t.Fatalf("collections = %d, want 2 (%s)", len(colls), raw)
	}
	a, b := colls[0], colls[1]
	if a["mix_id"] != "mock_mix_1" || b["mix_id"] != "mock_mix_2" {
		t.Fatalf("mix ids = %v / %v", a["mix_id"], b["mix_id"])
	}
	// B owns multiples of 15 that are not multiples of 10 (A wins ties): 17.
	if a["works_count"].(float64) != 50 || b["works_count"].(float64) != 17 {
		t.Fatalf("works_count = %v / %v, want 50 / 17", a["works_count"], b["works_count"])
	}

	// 6. Collection works endpoint shares the list contract.
	collAID := int64(a["id"].(float64))
	status, raw = getJSON(t, client,
		fmt.Sprintf("%s/api/collections/%d/works?page_size=50", server.URL, collAID))
	if status != http.StatusOK {
		t.Fatalf("GET collection works = %d %s", status, raw)
	}
	var cpage struct {
		Total float64          `json:"total"`
		Items []map[string]any `json:"items"`
	}
	json.Unmarshal(raw, &cpage)
	if cpage.Total != 50 || len(cpage.Items) != 50 {
		t.Fatalf("collection works = total %v items %d, want 50/50", cpage.Total, len(cpage.Items))
	}

	// 7. batch-ids -> exactly 502 ids.
	status, raw = postJSON(t, client, server.URL+"/api/works/batch-ids",
		fmt.Sprintf(`{"creator_id":%d}`, created.CreatorID))
	if status != http.StatusOK {
		t.Fatalf("batch-ids = %d %s", status, raw)
	}
	var batch struct {
		IDs []float64 `json:"ids"`
	}
	if err := json.Unmarshal(raw, &batch); err != nil {
		t.Fatal(err)
	}
	if len(batch.IDs) != 502 {
		t.Fatalf("batch ids = %d, want 502", len(batch.IDs))
	}
	// Filtered batch (collection B: 17).
	status, raw = postJSON(t, client, server.URL+"/api/works/batch-ids",
		fmt.Sprintf(`{"creator_id":%d,"collection_id":%d}`, created.CreatorID, int64(b["id"].(float64))))
	json.Unmarshal(raw, &batch)
	if status != http.StatusOK || len(batch.IDs) != 17 {
		t.Fatalf("filtered batch = %d ids (status %d), want 17", len(batch.IDs), status)
	}

	// 8. Work detail: mix_info on a collection work + qualities via mock.
	workAID := int64(cpage.Items[0]["id"].(float64))
	status, raw = getJSON(t, client, fmt.Sprintf("%s/api/works/%d", server.URL, workAID))
	if status != http.StatusOK {
		t.Fatalf("GET work = %d %s", status, raw)
	}
	var detailWork map[string]any
	json.Unmarshal(raw, &detailWork)
	if detailWork["mix_info"] == nil || detailWork["last_job"] != nil {
		t.Fatalf("work detail mix_info=%v last_job=%v", detailWork["mix_info"], detailWork["last_job"])
	}
	status, raw = getJSON(t, client, fmt.Sprintf("%s/api/works/%d/qualities", server.URL, workAID))
	if status != http.StatusOK {
		t.Fatalf("qualities = %d %s, want 200 via mock", status, raw)
	}
	var qualities []map[string]any
	json.Unmarshal(raw, &qualities)
	if len(qualities) != 3 || qualities[0]["quality"] != "1080p" {
		t.Fatalf("qualities = %s", raw)
	}

	// 9. Subscriptions CRUD round-trip.
	status, raw = postJSON(t, client, server.URL+"/api/subscriptions",
		fmt.Sprintf(`{"target_type":"collection","creator_id":%d,"collection_id":%d,"interval_minutes":60,"auto_download":true,"quality":"720p"}`,
			created.CreatorID, collAID))
	if status != http.StatusOK {
		t.Fatalf("POST subscription = %d %s", status, raw)
	}
	var sub map[string]any
	json.Unmarshal(raw, &sub)
	subID := int64(sub["id"].(float64))
	if sub["target_name"] != "Mock合集A" || sub["quality"] != "720p" || sub["enabled"] != true {
		t.Fatalf("subscription = %s", raw)
	}
	if sub["next_run_at"] == nil {
		t.Fatalf("next_run_at missing: %s", raw)
	}
	status, raw = getJSON(t, client, server.URL+"/api/subscriptions")
	if status != http.StatusOK || !strings.Contains(string(raw), `"target_type":"collection"`) {
		t.Fatalf("GET subscriptions = %d %s", status, raw)
	}
	status, raw = patchJSON(t, client, fmt.Sprintf("%s/api/subscriptions/%d", server.URL, subID),
		`{"interval_minutes":30,"auto_download":false}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH subscription = %d %s", status, raw)
	}
	var patched map[string]any
	json.Unmarshal(raw, &patched)
	if patched["interval_minutes"].(float64) != 30 || patched["auto_download"] != false {
		t.Fatalf("patched = %s", raw)
	}
	// Invalid creates are rejected.
	status, _ = postJSON(t, client, server.URL+"/api/subscriptions",
		`{"target_type":"collection","creator_id":1,"interval_minutes":60}`)
	if status != http.StatusBadRequest {
		t.Fatalf("collection sub without collection_id = %d, want 400", status)
	}
	status, _ = postJSON(t, client, server.URL+"/api/subscriptions",
		fmt.Sprintf(`{"target_type":"creator","creator_id":%d,"interval_minutes":0}`, created.CreatorID))
	if status != http.StatusBadRequest {
		t.Fatalf("zero interval = %d, want 400", status)
	}
	status, raw = deleteJSON(t, client, fmt.Sprintf("%s/api/subscriptions/%d", server.URL, subID))
	if status != http.StatusOK {
		t.Fatalf("DELETE subscription = %d %s", status, raw)
	}
	// DELETE is not idempotent: the second call answers 404.
	status, _ = deleteJSON(t, client, fmt.Sprintf("%s/api/subscriptions/%d", server.URL, subID))
	if status != http.StatusNotFound {
		t.Fatalf("second DELETE = %d, want 404", status)
	}

	// 10a. Re-adding the same creator returns the same id and triggers a new
	// scan (single-flight folds it into any running one).
	status, raw = postJSON(t, client, server.URL+"/api/creators", `{"profile_url":"MS4wLjABAAAA_e2e_mock"}`)
	if status != http.StatusAccepted {
		t.Fatalf("re-add = %d %s, want 202", status, raw)
	}
	var again struct {
		CreatorID int64 `json:"creator_id"`
	}
	if err := json.Unmarshal(raw, &again); err != nil || again.CreatorID != created.CreatorID {
		t.Fatalf("re-add body %s (err %v), want creator_id %d", raw, err, created.CreatorID)
	}

	// 10. rescan endpoint -> 202.
	status, raw = postJSON(t, client,
		fmt.Sprintf("%s/api/creators/%d/rescan", server.URL, created.CreatorID), `{}`)
	if status != http.StatusAccepted {
		t.Fatalf("rescan = %d %s, want 202", status, raw)
	}

	// 11. DELETE the creator (allowed: no scan running after rescan completes;
	// but the rescan just started - give it a moment and tolerate 409).
	time.Sleep(2 * time.Second)
	status, raw = deleteJSON(t, client, fmt.Sprintf("%s/api/creators/%d", server.URL, created.CreatorID))
	if status == http.StatusConflict {
		t.Logf("creator delete skipped, scan still running (accepted race): %s", raw)
		return
	}
	if status != http.StatusOK {
		t.Fatalf("DELETE creator = %d %s", status, raw)
	}
	status, raw = getJSON(t, client, fmt.Sprintf("%s/api/creators/%d", server.URL, created.CreatorID))
	if status != http.StatusNotFound {
		t.Fatalf("GET deleted creator = %d %s, want 404", status, raw)
	}
}

func deleteJSON(t *testing.T, client *http.Client, url string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}
