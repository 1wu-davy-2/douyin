package api

// Acceptance for contract v1.2: works type=/dl= filters (shared by the works
// lists and batch-ids), creator download_bytes and the subscription monitor
// stats new_works / new_downloaded.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"testing"
	"time"
)

// seedFilterWorks inserts a creator whose works cover every dl_status
// derivation (none / queued / paused_q -> queued / downloading / succeeded /
// failed / canceled) and both types including a live gallery. It returns the
// creator id plus a label->work-id map:
//
//	none        video work, no jobs
//	queued      video work, latest job queued
//	queuedpq    plain gallery, latest job paused_q (folds into queued)
//	downloading live gallery (image + live0001 clip), latest job downloading
//	succeeded   video work, failed job then succeeded job (latest wins)
//	failed      plain gallery, latest job failed
//	canceled    video work, latest job canceled
func seedFilterWorks(t *testing.T, h *sql.DB) (int64, map[string]int64) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := h.Exec(
		`INSERT INTO creators (sec_uid, nickname, profile_url, created_at)
		 VALUES ('MS4wLjABAAAAapi_fltr', '筛选博主', 'u', ?)`, now)
	if err != nil {
		t.Fatal(err)
	}
	creatorID, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}

	works := []struct {
		label, itemID, title, workType string
		live                           bool
		jobStatus                      string // "" -> no job; two statuses -> latest wins
		job2                           string
	}{
		{"none", "fl_none", "walnut none", "video", false, "", ""},
		{"queued", "fl_queued", "apricot queued", "video", false, "queued", ""},
		{"queuedpq", "fl_pq", "cherry gallery", "image", false, "paused_q", ""},
		{"downloading", "fl_dl", "peach live", "image", true, "downloading", ""},
		{"succeeded", "fl_ok", "plum succeeded", "video", false, "failed", "succeeded"},
		{"failed", "fl_fail", "mango failed", "image", false, "failed", ""},
		{"canceled", "fl_cancel", "kiwi canceled", "video", false, "canceled", ""},
	}
	ids := make(map[string]int64, len(works))
	for _, wk := range works {
		res, err := h.Exec(
			`INSERT INTO works (creator_id, item_id, title, type, duration, published_at, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			creatorID, wk.itemID, wk.title, wk.workType,
			map[bool]int{true: 30, false: 0}[wk.workType == "video"], now, now, now)
		if err != nil {
			t.Fatal(err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		ids[wk.label] = id
		if wk.workType == "image" {
			for _, a := range []struct{ kind, quality string }{
				{"cover", ""},
				{"metadata", ""},
				{"image", "0001"},
				{"image", "0002"},
			} {
				if _, err := h.Exec(
					`INSERT INTO assets (work_id, kind, path, size_bytes, quality, created_at)
					 VALUES (?, ?, ?, 10, ?, ?)`,
					id, a.kind, fmt.Sprintf("downloads/c/singles/%s/%s", wk.itemID, a.kind), nullIfEmpty(a.quality), now); err != nil {
					t.Fatal(err)
				}
			}
		}
		if wk.live {
			if _, err := h.Exec(
				`INSERT INTO assets (work_id, kind, path, size_bytes, quality, created_at)
				 VALUES (?, 'video', ?, 10, 'live0001', ?)`,
				id, "downloads/c/singles/"+wk.itemID+"/live0001.mp4", now); err != nil {
				t.Fatal(err)
			}
		}
		for _, st := range []string{wk.jobStatus, wk.job2} {
			if st == "" {
				continue
			}
			if _, err := h.Exec(
				`INSERT INTO download_jobs (work_id, creator_id, quality, status, queued_at)
				 VALUES (?, ?, '1080p', ?, ?)`, id, creatorID, st, now); err != nil {
				t.Fatal(err)
			}
		}
	}
	return creatorID, ids
}

// worksListIDs GETs a works page and returns the item ids (page_size=100
// covers every seeded work).
func worksListIDs(t *testing.T, client *http.Client, url string) []int64 {
	t.Helper()
	status, raw := getJSON(t, client, url)
	if status != http.StatusOK {
		t.Fatalf("GET %s = %d %s", url, status, raw)
	}
	var page struct {
		Total int64            `json:"total"`
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("decode %s: %v (%s)", url, err, raw)
	}
	if page.Total != int64(len(page.Items)) {
		t.Fatalf("%s: total %d != items %d", url, page.Total, len(page.Items))
	}
	ids := make([]int64, 0, len(page.Items))
	for _, it := range page.Items {
		ids = append(ids, int64(it["id"].(float64)))
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func batchIDs(t *testing.T, client *http.Client, url, body string) []int64 {
	t.Helper()
	status, raw := postJSON(t, client, url, body)
	if status != http.StatusOK {
		t.Fatalf("POST %s = %d %s", url, status, raw)
	}
	var out struct {
		IDs []int64 `json:"ids"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v (%s)", url, err, raw)
	}
	sort.Slice(out.IDs, func(i, j int) bool { return out.IDs[i] < out.IDs[j] })
	return out.IDs
}

func assertIDs(t *testing.T, got []int64, want ...int64) {
	t.Helper()
	wantSorted := append([]int64(nil), want...)
	sort.Slice(wantSorted, func(i, j int) bool { return wantSorted[i] < wantSorted[j] })
	if len(got) != len(wantSorted) {
		t.Fatalf("ids = %v, want %v", got, wantSorted)
	}
	for i := range got {
		if got[i] != wantSorted[i] {
			t.Fatalf("ids = %v, want %v", got, wantSorted)
		}
	}
}

func TestWorksListTypeAndDlFilters(t *testing.T) {
	server, _, database := newTestServer(t)
	client := setupAdmin(t, server)
	creatorID, ids := seedFilterWorks(t, database)
	now := time.Now().UTC().Format(time.RFC3339)
	base := fmt.Sprintf("%s/api/creators/%d/works?page_size=100", server.URL, creatorID)
	all := func() []int64 {
		return []int64{ids["none"], ids["queued"], ids["queuedpq"], ids["downloading"],
			ids["succeeded"], ids["failed"], ids["canceled"]}
	}

	// Unfiltered: everything.
	assertIDs(t, worksListIDs(t, client, base), all()...)

	// type= filters.
	assertIDs(t, worksListIDs(t, client, base+"&type=video"),
		ids["none"], ids["queued"], ids["succeeded"], ids["canceled"])
	assertIDs(t, worksListIDs(t, client, base+"&type=image"),
		ids["queuedpq"], ids["downloading"], ids["failed"])
	// live: image work carrying a live video clip only.
	assertIDs(t, worksListIDs(t, client, base+"&type=live"), ids["downloading"])
	// Unknown type values are ignored.
	assertIDs(t, worksListIDs(t, client, base+"&type=bogus"), all()...)

	// dl= filters (latest job derivation; paused_q folds into queued).
	assertIDs(t, worksListIDs(t, client, base+"&dl=none"), ids["none"])
	assertIDs(t, worksListIDs(t, client, base+"&dl=queued"), ids["queued"], ids["queuedpq"])
	assertIDs(t, worksListIDs(t, client, base+"&dl=downloading"), ids["downloading"])
	assertIDs(t, worksListIDs(t, client, base+"&dl=succeeded"), ids["succeeded"])
	assertIDs(t, worksListIDs(t, client, base+"&dl=failed"), ids["failed"])
	// canceled is not a contract filter value -> ignored.
	assertIDs(t, worksListIDs(t, client, base+"&dl=canceled"), all()...)
	assertIDs(t, worksListIDs(t, client, base+"&dl=bogus"), all()...)

	// Orthogonal combinations.
	assertIDs(t, worksListIDs(t, client, base+"&type=image&dl=queued"), ids["queuedpq"])
	assertIDs(t, worksListIDs(t, client, base+"&type=video&dl=succeeded"), ids["succeeded"])
	assertIDs(t, worksListIDs(t, client, base+"&q=apricot&dl=queued"), ids["queued"])
	assertIDs(t, worksListIDs(t, client, base+"&q=apricot&dl=failed"))
	// q + sort still compose with the filters.
	assertIDs(t, worksListIDs(t, client, base+"&type=live&sort=duration_desc"), ids["downloading"])

	// dl=none means "no job row", not "no succeeded assets": add succeeded
	// assets to the jobless work and confirm it still counts as none.
	if _, err := database.Exec(
		`INSERT INTO assets (work_id, kind, path, size_bytes, quality, created_at)
		 VALUES (?, 'video', 'x/none/1080p.mp4', 10, '1080p', ?)`,
		ids["none"], time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	assertIDs(t, worksListIDs(t, client, base+"&dl=none"), ids["none"])

	// The collection works endpoint shares the filter contract.
	res, err := database.Exec(
		`INSERT INTO collections (creator_id, mix_id, name, cover_url, created_at)
		 VALUES (?, 'fl_mix', '筛选合集', '', ?)`, creatorID, now)
	if err != nil {
		t.Fatal(err)
	}
	collID, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	for _, lbl := range []string{"queuedpq", "downloading"} {
		if _, err := database.Exec(`UPDATE works SET collection_id = ? WHERE id = ?`,
			collID, ids[lbl]); err != nil {
			t.Fatal(err)
		}
	}
	collBase := fmt.Sprintf("%s/api/collections/%d/works?page_size=100", server.URL, collID)
	assertIDs(t, worksListIDs(t, client, collBase), ids["queuedpq"], ids["downloading"])
	assertIDs(t, worksListIDs(t, client, collBase+"&dl=queued"), ids["queuedpq"])
	assertIDs(t, worksListIDs(t, client, collBase+"&type=live"), ids["downloading"])
	// creator list + collection_id param keeps the same behaviour.
	assertIDs(t, worksListIDs(t, client,
		fmt.Sprintf("%s&collection_id=%d&dl=queued", base, collID)), ids["queuedpq"])
}

// TestBatchIDsMatchesListFilter proves batch-ids and the works list agree for
// every filter combination ("select all matching" must select what the user
// sees).
func TestBatchIDsMatchesListFilter(t *testing.T) {
	server, _, database := newTestServer(t)
	client := setupAdmin(t, server)
	creatorID, _ := seedFilterWorks(t, database)
	url := server.URL + "/api/works/batch-ids"
	listBase := fmt.Sprintf("%s/api/creators/%d/works?page_size=100", server.URL, creatorID)

	for _, tc := range []struct{ typ, dl string }{
		{"", ""}, {"video", ""}, {"image", ""}, {"live", ""},
		{"", "none"}, {"", "queued"}, {"", "downloading"}, {"", "succeeded"}, {"", "failed"},
		{"video", "succeeded"}, {"image", "queued"}, {"video", "none"}, {"live", "downloading"},
		{"bogus", "alsobogus"}, {"image", "canceled"},
	} {
		body := fmt.Sprintf(`{"creator_id":%d`, creatorID)
		if tc.typ != "" {
			body += fmt.Sprintf(`,"type":%q`, tc.typ)
		}
		if tc.dl != "" {
			body += fmt.Sprintf(`,"dl":%q`, tc.dl)
		}
		body += "}"
		want := worksListIDs(t, client, listBase+fmt.Sprintf("&type=%s&dl=%s", tc.typ, tc.dl))
		got := batchIDs(t, client, url, body)
		if len(got) != len(want) {
			t.Fatalf("type=%q dl=%q: batch %v != list %v", tc.typ, tc.dl, got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("type=%q dl=%q: batch %v != list %v", tc.typ, tc.dl, got, want)
			}
		}
	}
}

// TestCreatorDownloadBytes checks the v1.2 download_bytes sum on both list and
// detail: SUM(assets.size_bytes) over kind video+image for the creator's
// works (covers and metadata excluded; a creator without assets reports 0).
func TestCreatorDownloadBytes(t *testing.T) {
	server, _, database := newTestServer(t)
	client := setupAdmin(t, server)
	now := time.Now().UTC().Format(time.RFC3339)

	mkCreator := func(secUID string) int64 {
		res, err := database.Exec(
			`INSERT INTO creators (sec_uid, nickname, profile_url, created_at)
			 VALUES (?, '字节博主', 'u', ?)`, secUID, now)
		if err != nil {
			t.Fatal(err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	mkWork := func(creatorID int64, itemID string) int64 {
		res, err := database.Exec(
			`INSERT INTO works (creator_id, item_id, title, type, created_at, updated_at)
			 VALUES (?, ?, ?, 'video', ?, ?)`, creatorID, itemID, itemID, now, now)
		if err != nil {
			t.Fatal(err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	mkAsset := func(workID int64, kind, quality string, size int64) {
		if _, err := database.Exec(
			`INSERT INTO assets (work_id, kind, path, size_bytes, quality, created_at)
			 VALUES (?, ?, 'p', ?, ?, ?)`, workID, kind, size, nullIfEmpty(quality), now); err != nil {
			t.Fatal(err)
		}
	}

	c1 := mkCreator("MS4wLjABAAAAapi_bytes")
	v := mkWork(c1, "b_vid") // video work: two quality tiers = 100+200
	mkAsset(v, "video", "720p", 100)
	mkAsset(v, "video", "1080p", 200)
	g := mkWork(c1, "b_gal") // gallery work: images + live clip count, cover/metadata do not
	for _, a := range []struct {
		kind, quality string
		size          int64
	}{
		{"image", "0001", 10}, {"image", "0002", 10}, {"image", "0003", 10},
		{"video", "live0001", 30},
		{"cover", "", 5}, {"metadata", "", 7},
	} {
		mkAsset(g, a.kind, a.quality, a.size)
	}
	// A soft-deleted work keeps its files on disk, so its bytes still count.
	d := mkWork(c1, "b_deleted")
	mkAsset(d, "video", "720p", 999)
	if _, err := database.Exec(`UPDATE works SET deleted_at = ? WHERE id = ?`, now, d); err != nil {
		t.Fatal(err)
	}
	want := int64(300 + 60 + 999)
	c2 := mkCreator("MS4wLjABAAAAapi_bare") // no works/assets at all

	status, raw := getJSON(t, client, server.URL+"/api/creators")
	if status != http.StatusOK {
		t.Fatalf("creators list = %d %s", status, raw)
	}
	var list []map[string]any
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	got := map[int64]float64{}
	for _, it := range list {
		if _, ok := it["download_bytes"]; !ok {
			t.Fatalf("creator list item missing download_bytes: %v", it)
		}
		got[int64(it["id"].(float64))] = it["download_bytes"].(float64)
	}
	if got[c1] != float64(want) {
		t.Fatalf("creator %d download_bytes = %v, want %d", c1, got[c1], want)
	}
	if got[c2] != 0 {
		t.Fatalf("creator %d download_bytes = %v, want 0", c2, got[c2])
	}

	status, raw = getJSON(t, client, fmt.Sprintf("%s/api/creators/%d", server.URL, c1))
	if status != http.StatusOK {
		t.Fatalf("creator detail = %d %s", status, raw)
	}
	var detail map[string]any
	if err := json.Unmarshal(raw, &detail); err != nil {
		t.Fatal(err)
	}
	if detail["download_bytes"].(float64) != float64(want) {
		t.Fatalf("detail download_bytes = %v, want %d", detail["download_bytes"], want)
	}
}

// TestSubscriptionMonitorStats checks new_works / new_downloaded for both
// target types: creator targets count the whole creator, collection targets
// only their collection; only works recorded after the subscription was
// created count, and new_downloaded requires the latest job to be succeeded.
func TestSubscriptionMonitorStats(t *testing.T) {
	server, _, database := newTestServer(t)
	client := setupAdmin(t, server)

	const (
		before = "2026-01-01T00:00:00Z"
		subAt  = "2026-02-01T00:00:00Z"
		after  = "2026-03-01T00:00:00Z"
	)
	res, err := database.Exec(
		`INSERT INTO creators (sec_uid, nickname, profile_url, created_at)
		 VALUES ('MS4wLjABAAAAapi_sub', '订阅博主', 'u', ?)`, before)
	if err != nil {
		t.Fatal(err)
	}
	creatorID, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	res, err = database.Exec(
		`INSERT INTO collections (creator_id, mix_id, name, cover_url, created_at)
		 VALUES (?, 'sub_mix', '订阅合集', '', ?)`, creatorID, before)
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
			`INSERT INTO works (creator_id, collection_id, item_id, title, type, created_at, updated_at)
			 VALUES (?, ?, ?, ?, 'video', ?, ?)`,
			creatorID, coll, itemID, itemID, createdAt, createdAt)
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
	// Singles: old+downloaded (not new), new+failed->succeeded (new+downloaded),
	// new+queued (new, not downloaded), new+no job (new, not downloaded).
	mkWork("s_old", "", before, "succeeded")
	mkWork("s_new_ok", "", after, "failed", "succeeded")
	mkWork("s_new_q", "", after, "queued")
	mkWork("s_new_none", "", after)
	// Collection works: one old (not new), one new+downloaded.
	mkWork("k_old", "coll", before)
	mkWork("k_new", "coll", after, "succeeded")

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
	byTarget := map[string]map[string]any{}
	for _, sub := range subs {
		for _, field := range []string{"new_works", "new_downloaded"} {
			if _, ok := sub[field]; !ok {
				t.Fatalf("subscription missing %s: %v", field, sub)
			}
		}
		byTarget[sub["target_type"].(string)] = sub
	}
	// Creator target: new works = s_new_ok + s_new_q + s_new_none + k_new = 4,
	// of which s_new_ok and k_new are succeeded -> 2.
	if got := byTarget["creator"]["new_works"].(float64); got != 4 {
		t.Fatalf("creator target new_works = %v, want 4 (%s)", got, raw)
	}
	if got := byTarget["creator"]["new_downloaded"].(float64); got != 2 {
		t.Fatalf("creator target new_downloaded = %v, want 2", got)
	}
	// Collection target: only k_new is new and downloaded.
	if got := byTarget["collection"]["new_works"].(float64); got != 1 {
		t.Fatalf("collection target new_works = %v, want 1", got)
	}
	if got := byTarget["collection"]["new_downloaded"].(float64); got != 1 {
		t.Fatalf("collection target new_downloaded = %v, want 1", got)
	}

	// A subscription created just now has no newer works: both stats are 0.
	status, raw = postJSON(t, client, server.URL+"/api/subscriptions",
		fmt.Sprintf(`{"target_type":"creator","creator_id":%d,"interval_minutes":60}`, creatorID))
	if status != http.StatusOK {
		t.Fatalf("POST subscription = %d %s", status, raw)
	}
	var fresh map[string]any
	if err := json.Unmarshal(raw, &fresh); err != nil {
		t.Fatal(err)
	}
	if fresh["new_works"].(float64) != 0 || fresh["new_downloaded"].(float64) != 0 {
		t.Fatalf("fresh subscription stats = %s, want 0/0", raw)
	}
}
