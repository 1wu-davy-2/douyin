package api

// Tests for the subscription edit surface (PATCH /api/subscriptions/{id}) and
// the monitor-period works detail (GET /api/subscriptions/{id}/new-works),
// both exercised end-to-end against a mock-mode test server.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// TestSubscriptionPatchFullUpdate covers full and partial updates of every
// editable subscription field (interval_minutes, quality, auto_download,
// enabled) plus validation errors.
func TestSubscriptionPatchFullUpdate(t *testing.T) {
	server, _, database := newTestServer(t)
	client := setupAdmin(t, server)

	res, err := database.Exec(
		`INSERT INTO creators (sec_uid, nickname, profile_url, created_at)
		 VALUES ('MS4wLjABAAAAsub_patch', '补丁博主', 'u', '2026-01-01T00:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	creatorID, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}

	status, raw := postJSON(t, client, server.URL+"/api/subscriptions",
		fmt.Sprintf(`{"target_type":"creator","creator_id":%d,"interval_minutes":60,"auto_download":true,"quality":"1080p"}`, creatorID))
	if status != http.StatusOK {
		t.Fatalf("POST subscription = %d %s", status, raw)
	}
	var created map[string]any
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	subID := int(created["id"].(float64))

	// Full edit: every editable field at once.
	status, raw = patchJSON(t, client, fmt.Sprintf("%s/api/subscriptions/%d", server.URL, subID),
		`{"interval_minutes":30,"quality":"720p","auto_download":false,"enabled":false}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH full = %d %s", status, raw)
	}
	var patched map[string]any
	if err := json.Unmarshal(raw, &patched); err != nil {
		t.Fatal(err)
	}
	for field, want := range map[string]any{
		"interval_minutes": float64(30),
		"quality":          "720p",
		"auto_download":    false,
		"enabled":          false,
	} {
		if got := patched[field]; got != want {
			t.Fatalf("PATCH full: %s = %v, want %v (%s)", field, got, want, raw)
		}
	}
	// Target fields must survive untouched.
	if patched["target_type"] != "creator" || patched["creator_id"].(float64) != float64(creatorID) {
		t.Fatalf("PATCH full changed target: %s", raw)
	}

	// Partial edit: only quality moves; the rest keeps its stored value.
	status, raw = patchJSON(t, client, fmt.Sprintf("%s/api/subscriptions/%d", server.URL, subID),
		`{"quality":"540p"}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH partial = %d %s", status, raw)
	}
	if err := json.Unmarshal(raw, &patched); err != nil {
		t.Fatal(err)
	}
	for field, want := range map[string]any{
		"quality":          "540p",
		"interval_minutes": float64(30),
		"auto_download":    false,
		"enabled":          false,
	} {
		if got := patched[field]; got != want {
			t.Fatalf("PATCH partial: %s = %v, want %v (%s)", field, got, want, raw)
		}
	}

	// Validation errors.
	for _, tc := range []struct {
		name   string
		body   string
		status int
	}{
		{"bad quality", `{"quality":"4320p"}`, http.StatusBadRequest},
		{"zero interval", `{"interval_minutes":0}`, http.StatusBadRequest},
		{"negative interval", `{"interval_minutes":-5}`, http.StatusBadRequest},
	} {
		status, raw = patchJSON(t, client, fmt.Sprintf("%s/api/subscriptions/%d", server.URL, subID), tc.body)
		if status != tc.status {
			t.Fatalf("PATCH %s = %d %s, want %d", tc.name, status, raw, tc.status)
		}
	}

	// Unknown subscription id -> 404.
	status, _ = patchJSON(t, client, server.URL+"/api/subscriptions/999999", `{"enabled":true}`)
	if status != http.StatusNotFound {
		t.Fatalf("PATCH unknown id = %d, want 404", status)
	}

	// The stored row really changed (read back through the list endpoint).
	status, raw = getJSON(t, client, server.URL+"/api/subscriptions")
	if status != http.StatusOK {
		t.Fatalf("GET subscriptions = %d %s", status, raw)
	}
	var subs []map[string]any
	if err := json.Unmarshal(raw, &subs); err != nil {
		t.Fatal(err)
	}
	if len(subs) != 1 {
		t.Fatalf("subscriptions = %d rows, want 1", len(subs))
	}
	if subs[0]["quality"] != "540p" || subs[0]["interval_minutes"].(float64) != 30 ||
		subs[0]["auto_download"] != false || subs[0]["enabled"] != false {
		t.Fatalf("stored subscription after edits = %s", raw)
	}
}

// TestSubscriptionNewWorks checks GET /api/subscriptions/{id}/new-works: the
// item list matches the list-endpoint monitor stats (same scope, same
// succeeded-derivation), works are newest-published first and the item shape
// equals the works lists.
func TestSubscriptionNewWorks(t *testing.T) {
	server, _, database := newTestServer(t)
	client := setupAdmin(t, server)

	const (
		before = "2026-01-01T00:00:00Z"
		subAt  = "2026-02-01T00:00:00Z"
		after  = "2026-03-01T00:00:00Z"
	)
	res, err := database.Exec(
		`INSERT INTO creators (sec_uid, nickname, profile_url, created_at)
		 VALUES ('MS4wLjABAAAAapi_newworks', '明细博主', 'u', ?)`, before)
	if err != nil {
		t.Fatal(err)
	}
	creatorID, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	res, err = database.Exec(
		`INSERT INTO collections (creator_id, mix_id, name, cover_url, created_at)
		 VALUES (?, 'nw_mix', '明细合集', '', ?)`, creatorID, before)
	if err != nil {
		t.Fatal(err)
	}
	collID, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}

	mkWork := func(itemID, collection, createdAt string, jobs ...string) int64 {
		var coll any
		if collection != "" {
			coll = collID
		}
		wres, err := database.Exec(
			`INSERT INTO works (creator_id, collection_id, item_id, title, type, published_at, created_at, updated_at)
			 VALUES (?, ?, ?, ?, 'video', ?, ?, ?)`,
			creatorID, coll, itemID, itemID, createdAt, createdAt, createdAt)
		if err != nil {
			t.Fatal(err)
		}
		id, err := wres.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		for _, st := range jobs {
			if _, err := database.Exec(
				`INSERT INTO download_jobs (work_id, creator_id, quality, status, queued_at)
				 VALUES (?, ?, '1080p', ?, ?)`, id, creatorID, st, createdAt); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	// Same fixture as TestSubscriptionMonitorStats: the new-works endpoint
	// must agree with those list stats.
	sOld := mkWork("s_old", "", before, "succeeded")
	sNewOK := mkWork("s_new_ok", "", after, "failed", "succeeded")
	sNewQ := mkWork("s_new_q", "", after, "queued")
	mkWork("s_new_none", "", after)
	mkWork("k_old", "coll", before)
	kNew := mkWork("k_new", "coll", after, "succeeded")

	for _, sub := range []struct {
		target     string
		collection any
	}{
		{"creator", nil},
		{"collection", collID},
	} {
		if _, err := database.Exec(
			`INSERT INTO subscriptions (target_type, creator_id, collection_id, interval_minutes,
			                            auto_download, quality, enabled, created_at)
			 VALUES (?, ?, ?, 60, 0, '1080p', 1, ?)`,
			sub.target, creatorID, sub.collection, subAt); err != nil {
			t.Fatal(err)
		}
	}

	status, raw := getJSON(t, client, server.URL+"/api/subscriptions")
	if status != http.StatusOK {
		t.Fatalf("subscriptions = %d %s", status, raw)
	}
	var subs []map[string]any
	if err := json.Unmarshal(raw, &subs); err != nil {
		t.Fatal(err)
	}
	subIDByTarget := map[string]float64{}
	for _, sub := range subs {
		subIDByTarget[sub["target_type"].(string)] = sub["id"].(float64)
	}

	fetch := func(subID float64) map[string]any {
		t.Helper()
		st, raw := getJSON(t, client, fmt.Sprintf("%s/api/subscriptions/%d/new-works", server.URL, int64(subID)))
		if st != http.StatusOK {
			t.Fatalf("GET new-works = %d %s", st, raw)
		}
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	// Creator target: 4 new works, 2 downloaded, matching the list stats.
	creatorResp := fetch(subIDByTarget["creator"])
	if got := creatorResp["total"].(float64); got != 4 {
		t.Fatalf("creator target total = %v, want 4 (%s)", got, raw)
	}
	if got := creatorResp["downloaded"].(float64); got != 2 {
		t.Fatalf("creator target downloaded = %v, want 2", got)
	}
	items := creatorResp["items"].([]any)
	if len(items) != 4 {
		t.Fatalf("creator target items = %d, want 4", len(items))
	}
	first := items[0].(map[string]any)
	// Item shape must equal the works-list whitelist fields.
	for _, field := range []string{"id", "item_id", "title", "cover_url", "duration", "published_at",
		"collection_id", "collection_name", "type", "image_count", "dl_status", "downloaded_quality", "created_at"} {
		if _, ok := first[field]; !ok {
			t.Fatalf("new-works item missing %s: %v", field, first)
		}
	}
	// Newest published first: both new singles and the collection work share
	// created_at "after", so id DESC breaks the tie.
	if first["id"].(float64) != float64(kNew) {
		t.Fatalf("first item id = %v, want collection work %d", first["id"], kNew)
	}
	// dl_status / downloaded_quality derived from the latest job.
	byID := map[string]map[string]any{}
	for _, it := range items {
		m := it.(map[string]any)
		byID[m["item_id"].(string)] = m
	}
	if byID["s_old"] != nil {
		t.Fatal("pre-subscription work leaked into monitor-period items")
	}
	if got := byID["s_new_ok"]["dl_status"]; got != "succeeded" {
		t.Fatalf("s_new_ok dl_status = %v, want succeeded", got)
	}
	if got := byID["s_new_ok"]["downloaded_quality"]; got != "1080p" {
		t.Fatalf("s_new_ok downloaded_quality = %v, want 1080p", got)
	}
	if got := byID["s_new_q"]["dl_status"]; got != "queued" {
		t.Fatalf("s_new_q dl_status = %v, want queued", got)
	}
	if got := byID["s_new_none"]["dl_status"]; got != "none" {
		t.Fatalf("s_new_none dl_status = %v, want none", got)
	}
	if got := byID["k_new"]["collection_id"]; got == nil || got.(float64) != float64(collID) {
		t.Fatalf("k_new collection_id = %v, want %d", got, collID)
	}
	if got := byID["k_new"]["collection_name"]; got != "明细合集" {
		t.Fatalf("k_new collection_name = %v, want 明细合集", got)
	}

	// Collection target: only the new work of that collection.
	collResp := fetch(subIDByTarget["collection"])
	if got := collResp["total"].(float64); got != 1 {
		t.Fatalf("collection target total = %v, want 1", got)
	}
	if got := collResp["downloaded"].(float64); got != 1 {
		t.Fatalf("collection target downloaded = %v, want 1", got)
	}
	collItems := collResp["items"].([]any)
	if len(collItems) != 1 || collItems[0].(map[string]any)["id"].(float64) != float64(kNew) {
		t.Fatalf("collection target items = %s, want only k_new (%d)", raw, kNew)
	}

	// Pre-subscription works are really outside the window (sanity on ids).
	if float64(sOld) == 0 || float64(sNewOK) == 0 || float64(sNewQ) == 0 {
		t.Fatal("fixture work ids missing")
	}

	// Unknown subscription -> 404; fresh subscription -> empty window.
	status, _ = getJSON(t, client, server.URL+"/api/subscriptions/999999/new-works")
	if status != http.StatusNotFound {
		t.Fatalf("GET new-works unknown id = %d, want 404", status)
	}
	status, raw = postJSON(t, client, server.URL+"/api/subscriptions",
		fmt.Sprintf(`{"target_type":"creator","creator_id":%d,"interval_minutes":60}`, creatorID))
	if status != http.StatusOK {
		t.Fatalf("POST subscription = %d %s", status, raw)
	}
	var freshSub map[string]any
	if err := json.Unmarshal(raw, &freshSub); err != nil {
		t.Fatal(err)
	}
	fresh := fetch(freshSub["id"].(float64))
	if fresh["total"].(float64) != 0 || fresh["downloaded"].(float64) != 0 || len(fresh["items"].([]any)) != 0 {
		t.Fatalf("fresh subscription new-works = %s, want empty", raw)
	}
}
