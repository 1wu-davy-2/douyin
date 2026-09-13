package api

// Contract v1.4/1.4b tests: POST /api/creators optional parameters
// (download_root/subscribe/group/alias), the new PATCH /api/creators/{id}
// alias+group endpoint, the exclude_collection_ids works filter (list +
// batch-ids) and their round-trips.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// createCreatorWithBody POSTs a raw create body and returns the 202 payload.
func createCreatorWithBody(t *testing.T, client *http.Client, serverURL, body string) map[string]any {
	t.Helper()
	status, raw := postJSON(t, client, serverURL+"/api/creators", body)
	if status != http.StatusAccepted {
		t.Fatalf("POST /api/creators = %d %s, want 202", status, raw)
	}
	var created map[string]any
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatalf("decode 202 body: %v (%s)", err, raw)
	}
	return created
}

// creatorLevelSub returns the creator-level subscription of creatorID (nil
// when absent).
func creatorLevelSub(t *testing.T, client *http.Client, serverURL string, creatorID int64) map[string]any {
	t.Helper()
	status, raw := getJSON(t, client, serverURL+"/api/subscriptions")
	if status != http.StatusOK {
		t.Fatalf("GET subscriptions = %d %s", status, raw)
	}
	var subs []map[string]any
	if err := json.Unmarshal(raw, &subs); err != nil {
		t.Fatalf("decode subscriptions: %v (%s)", err, raw)
	}
	for _, s := range subs {
		if s["target_type"] == "creator" && int64(s["creator_id"].(float64)) == creatorID {
			return s
		}
	}
	return nil
}

func deleteCreatorLevelSub(t *testing.T, client *http.Client, serverURL string, creatorID int64) {
	t.Helper()
	sub := creatorLevelSub(t, client, serverURL, creatorID)
	if sub == nil {
		return
	}
	status, raw := deleteJSON(t, client, fmt.Sprintf("%s/api/subscriptions/%d", serverURL, int64(sub["id"].(float64))))
	if status != http.StatusOK {
		t.Fatalf("cleanup: DELETE subscription = %d %s", status, raw)
	}
}

func TestCreateCreatorWithOptions(t *testing.T) {
	server, _, database := newTestServer(t)
	client := setupAdmin(t, server)

	root := filepath.Join(t.TempDir(), "creator-root")
	created := createCreatorWithBody(t, client, server.URL, fmt.Sprintf(
		`{"profile_url":"MS4wLjABAAAA_opts_mock","download_root":%q,"subscribe":{"interval_minutes":60,"quality":"720p","auto_download":true},"group":"美食","alias":"小厨"}`,
		filepath.ToSlash(root)))

	creatorID := int64(created["creator_id"].(float64))
	if creatorID <= 0 {
		t.Fatalf("creator_id missing: %s", created)
	}

	// 202 body echoes the creator view with alias/group.
	creator, _ := created["creator"].(map[string]any)
	if creator == nil {
		t.Fatalf("202 body lacks creator: %s", created)
	}
	if creator["alias"] != "小厨" || creator["group"] != "美食" {
		t.Fatalf("creator alias/group = %v/%v, want 小厨/美食", creator["alias"], creator["group"])
	}

	// download_root: validated (absolute) and created eagerly.
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("download_root was not created: %v", err)
	}
	var storedRoot string
	if err := database.QueryRow(`SELECT download_root FROM creators WHERE id = ?`, creatorID).Scan(&storedRoot); err != nil {
		t.Fatal(err)
	}
	if storedRoot == "" {
		t.Fatalf("creators.download_root not persisted (want %s)", root)
	}

	// subscribe created a creator-level subscription: interval 60, quality
	// 720p (as given), auto_download true, enabled.
	sub := creatorLevelSub(t, client, server.URL, creatorID)
	if sub == nil {
		t.Fatalf("creator-level subscription missing")
	}
	if sub["interval_minutes"].(float64) != 60 || sub["quality"] != "720p" ||
		sub["auto_download"] != true || sub["enabled"] != true {
		t.Fatalf("subscription = %v", sub)
	}
	// Drop the subscription again so the finished scan does not enqueue the
	// whole mock catalog (the enqueue path is covered by the scanner tests).
	deleteCreatorLevelSub(t, client, server.URL, creatorID)

	// List/detail expose alias/group; nickname still present.
	detail := waitScanFinished(t, client, server.URL, creatorID, 30*time.Second)
	if detail["alias"] != "小厨" || detail["group"] != "美食" {
		t.Fatalf("detail alias/group = %v/%v", detail["alias"], detail["group"])
	}
	status, raw := getJSON(t, client, server.URL+"/api/creators")
	if status != http.StatusOK || !strings.Contains(string(raw), `"alias":"小厨"`) {
		t.Fatalf("GET creators = %d %s", status, raw)
	}

	// Re-add with different optional params: same creator, params re-applied.
	created = createCreatorWithBody(t, client, server.URL,
		`{"profile_url":"MS4wLjABAAAA_opts_mock","group":"摄影","alias":null}`)
	if int64(created["creator_id"].(float64)) != creatorID {
		t.Fatalf("re-add creator_id = %v, want %d", created["creator_id"], creatorID)
	}
	creator, _ = created["creator"].(map[string]any)
	if creator["group"] != "摄影" {
		t.Fatalf("re-add group = %v, want 摄影", creator["group"])
	}
	if v, ok := creator["alias"]; ok && v != nil {
		t.Fatalf("re-add alias = %v, want null (explicit clear)", v)
	}
	// Re-add without subscribe: the (deleted) subscription is not re-created.
	if s := creatorLevelSub(t, client, server.URL, creatorID); s != nil {
		t.Fatalf("subscription re-created without subscribe: %v", s)
	}

	// Invalid quality / interval / download_root are rejected up front.
	status, _ = postJSON(t, client, server.URL+"/api/creators",
		`{"profile_url":"MS4wLjABAAAA_opts_other","subscribe":{"interval_minutes":60,"quality":"4k"}}`)
	if status != http.StatusBadRequest {
		t.Fatalf("bad subscribe quality = %d, want 400", status)
	}
	status, _ = postJSON(t, client, server.URL+"/api/creators",
		`{"profile_url":"MS4wLjABAAAA_opts_other","subscribe":{"interval_minutes":0}}`)
	if status != http.StatusBadRequest {
		t.Fatalf("zero subscribe interval = %d, want 400", status)
	}
	status, _ = postJSON(t, client, server.URL+"/api/creators",
		`{"profile_url":"MS4wLjABAAAA_opts_other","download_root":"relative/path"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("relative download_root = %d, want 400", status)
	}
}

func TestCreateCreatorSubscribeDefaults(t *testing.T) {
	server, _, _ := newTestServer(t)
	client := setupAdmin(t, server)

	// subscribe without quality/auto_download: quality = global default
	// (1080p), auto_download = true (contract default).
	created := createCreatorWithBody(t, client, server.URL,
		`{"profile_url":"MS4wLjABAAAA_autodl_mock","subscribe":{"interval_minutes":30}}`)
	creatorID := int64(created["creator_id"].(float64))
	sub := creatorLevelSub(t, client, server.URL, creatorID)
	if sub == nil {
		t.Fatalf("creator-level subscription not created")
	}
	if sub["interval_minutes"].(float64) != 30 || sub["quality"] != "1080p" || sub["auto_download"] != true {
		t.Fatalf("default subscription = %v", sub)
	}

	// Re-add with subscribe: the existing subscription is refreshed.
	createCreatorWithBody(t, client, server.URL,
		`{"profile_url":"MS4wLjABAAAA_autodl_mock","subscribe":{"interval_minutes":45,"quality":"540p","auto_download":false}}`)
	sub = creatorLevelSub(t, client, server.URL, creatorID)
	if sub == nil {
		t.Fatalf("subscription vanished on re-add")
	}
	if sub["interval_minutes"].(float64) != 45 || sub["quality"] != "540p" || sub["auto_download"] != false {
		t.Fatalf("refreshed subscription = %v", sub)
	}
	deleteCreatorLevelSub(t, client, server.URL, creatorID)
}

func TestPatchCreatorAliasGroup(t *testing.T) {
	server, _, _ := newTestServer(t)
	client := setupAdmin(t, server)

	created := createCreatorWithBody(t, client, server.URL,
		`{"profile_url":"MS4wLjABAAAA_patch_mock","alias":"原名","group":"旅行"}`)
	creatorID := int64(created["creator_id"].(float64))

	url := fmt.Sprintf("%s/api/creators/%d", server.URL, creatorID)

	// Round-trip: rename only.
	status, raw := patchJSON(t, client, url, `{"alias":"新名字"}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH alias = %d %s", status, raw)
	}
	var res map[string]any
	json.Unmarshal(raw, &res)
	if res["ok"] != true || res["alias"] != "新名字" || res["group"] != "旅行" {
		t.Fatalf("PATCH alias response = %s", raw)
	}

	// Move group only.
	status, raw = patchJSON(t, client, url, `{"group":"摄影"}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH group = %d %s", status, raw)
	}
	json.Unmarshal(raw, &res)
	if res["alias"] != "新名字" || res["group"] != "摄影" {
		t.Fatalf("PATCH group response = %s", raw)
	}

	// The detail reflects both values.
	status, raw = getJSON(t, client, url)
	if status != http.StatusOK {
		t.Fatalf("GET creator = %d %s", status, raw)
	}
	var detail map[string]any
	json.Unmarshal(raw, &detail)
	if detail["alias"] != "新名字" || detail["group"] != "摄影" {
		t.Fatalf("detail after PATCH = %v/%v", detail["alias"], detail["group"])
	}

	// null clears; empty string clears as well (contract).
	status, raw = patchJSON(t, client, url, `{"alias":null}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH alias null = %d %s", status, raw)
	}
	json.Unmarshal(raw, &res)
	if res["alias"] != nil || res["group"] != "摄影" {
		t.Fatalf("PATCH alias null response = %s", raw)
	}
	status, raw = patchJSON(t, client, url, `{"group":""}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH group empty = %d %s", status, raw)
	}
	json.Unmarshal(raw, &res)
	if res["group"] != nil || res["alias"] != nil {
		t.Fatalf("PATCH group empty response = %s", raw)
	}

	// Alias stays cleared (null) in the stored row.
	status, raw = getJSON(t, client, url)
	if status != http.StatusOK || strings.Contains(string(raw), `"alias":"`) {
		t.Fatalf("detail after clears = %d %s", status, raw)
	}

	// Unknown id -> 404; empty body -> 400.
	status, _ = patchJSON(t, client, server.URL+"/api/creators/99999", `{"alias":"x"}`)
	if status != http.StatusNotFound {
		t.Fatalf("PATCH missing creator = %d, want 404", status)
	}
	status, _ = patchJSON(t, client, url, `{}`)
	if status != http.StatusBadRequest {
		t.Fatalf("PATCH empty body = %d, want 400", status)
	}
}

func TestExcludeCollectionIdsFilter(t *testing.T) {
	server, _, _ := newTestServer(t)
	client := setupAdmin(t, server)

	created := createCreatorWithBody(t, client, server.URL,
		`{"profile_url":"MS4wLjABAAAA_e2e_mock"}`)
	creatorID := int64(created["creator_id"].(float64))
	waitScanFinished(t, client, server.URL, creatorID, 30*time.Second)

	// Mock catalog: 502 works; collection A holds multiples of 10 (50),
	// collection B multiples of 15 not in A (17).
	status, raw := getJSON(t, client, fmt.Sprintf("%s/api/creators/%d/collections", server.URL, creatorID))
	if status != http.StatusOK {
		t.Fatalf("GET collections = %d %s", status, raw)
	}
	var colls []map[string]any
	json.Unmarshal(raw, &colls)
	if len(colls) != 2 {
		t.Fatalf("collections = %d, want 2 (%s)", len(colls), raw)
	}
	colA := int64(colls[0]["id"].(float64))
	colB := int64(colls[1]["id"].(float64))

	getTotal := func(query string) (int, int64) {
		st, rd := getJSON(t, client, fmt.Sprintf("%s/api/creators/%d/works?%s", server.URL, creatorID, query))
		if st != http.StatusOK {
			t.Fatalf("GET works?%s = %d %s", query, st, rd)
		}
		var page struct {
			Total int64 `json:"total"`
		}
		json.Unmarshal(rd, &page)
		return st, page.Total
	}

	// exclude A: 502-50 = 452; exclude A+B: only singles = 435.
	_, total := getTotal(fmt.Sprintf("exclude_collection_ids=%d", colA))
	if total != 452 {
		t.Fatalf("exclude A total = %d, want 452", total)
	}
	_, total = getTotal(fmt.Sprintf("exclude_collection_ids=%d,%d", colA, colB))
	if total != 435 {
		t.Fatalf("exclude A+B total = %d, want 435 (singles only)", total)
	}
	// Same count as the legacy singles filter (collection_id=none).
	_, noneTotal := getTotal("collection_id=none")
	if noneTotal != 435 {
		t.Fatalf("collection_id=none total = %d, want 435", noneTotal)
	}
	// Whitespace around ids is tolerated.
	_, total = getTotal(fmt.Sprintf("exclude_collection_ids=%d%%2C%%20%d", colA, colB))
	if total != 435 {
		t.Fatalf("exclude with spaces total = %d, want 435", total)
	}
	// Garbage list -> 400.
	status, _ = getJSON(t, client, fmt.Sprintf("%s/api/creators/%d/works?exclude_collection_ids=1,x", server.URL, creatorID))
	if status != http.StatusBadRequest {
		t.Fatalf("invalid exclude list = %d, want 400", status)
	}

	// batch-ids shares the filter (exclude as JSON array).
	batchTotal := func(body string) int64 {
		st, rd := postJSON(t, client, server.URL+"/api/works/batch-ids", body)
		if st != http.StatusOK {
			t.Fatalf("batch-ids = %d %s", st, rd)
		}
		var res struct {
			IDs []float64 `json:"ids"`
		}
		json.Unmarshal(rd, &res)
		return int64(len(res.IDs))
	}
	if got := batchTotal(fmt.Sprintf(`{"creator_id":%d,"exclude_collection_ids":[%d]}`, creatorID, colA)); got != 452 {
		t.Fatalf("batch-ids exclude A = %d, want 452", got)
	}
	if got := batchTotal(fmt.Sprintf(`{"creator_id":%d,"exclude_collection_ids":[%d,%d]}`, creatorID, colA, colB)); got != 435 {
		t.Fatalf("batch-ids exclude A+B = %d, want 435", got)
	}
	// dl filter composes with exclude (no succeeded downloads yet).
	if got := batchTotal(fmt.Sprintf(`{"creator_id":%d,"dl":"succeeded","exclude_collection_ids":[%d]}`, creatorID, colA)); got != 0 {
		t.Fatalf("batch-ids exclude A + dl=succeeded = %d, want 0", got)
	}
}
