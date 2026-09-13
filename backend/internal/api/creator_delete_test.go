package api

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDeleteCreatorWithFiles(t *testing.T) {
	server, _, database, cfg := newTestServerWithCfg(t)
	client := setupAdmin(t, server)

	now := time.Now().UTC().Format(time.RFC3339)
	res, err := database.Exec(
		`INSERT INTO creators (sec_uid, nickname, profile_url, created_at)
		 VALUES ('MS4wLjABAAAAapi_delfiles', '待删博主', 'u', ?)`, now)
	if err != nil {
		t.Fatal(err)
	}
	creatorID, _ := res.LastInsertId()

	absRoot, err := filepath.Abs(filepath.Join(cfg.DataDir, "downloads"))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(absRoot, "delcreator", "singles")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f1 := filepath.Join(dir, "keep.mp4")
	if err := os.WriteFile(f1, make([]byte, 64), 0o644); err != nil {
		t.Fatal(err)
	}

	wres, err := database.Exec(
		`INSERT INTO works (creator_id, item_id, title, type, created_at, updated_at)
		 VALUES (?, 'del_f1', 'x', 'video', ?, ?)`, creatorID, now, now)
	if err != nil {
		t.Fatal(err)
	}
	workID, _ := wres.LastInsertId()
	if _, err := database.Exec(
		`INSERT INTO assets (work_id, kind, path, size_bytes, created_at)
		 VALUES (?, 'video', ?, 64, ?)`, workID, f1, now); err != nil {
		t.Fatal(err)
	}

	// delete_files=false:文件保留
	req1, _ := http.NewRequest(http.MethodDelete, server.URL+fmt.Sprintf("/api/creators/%d?delete_files=false", creatorID), nil)
	resp, err := client.Do(req1)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d", resp.StatusCode)
	}
	if _, err := os.Stat(f1); err != nil {
		t.Fatalf("file should remain when delete_files=false: %v", err)
	}

	// 重建记录,再 delete_files=true:文件删除
	if _, err := database.Exec(
		`INSERT INTO creators (id, sec_uid, nickname, profile_url, created_at)
		 VALUES (?, 'MS4wLjABAAAAapi_delfiles', '待删博主', 'u', ?)`, creatorID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(
		`INSERT INTO works (id, creator_id, item_id, title, type, created_at, updated_at)
		 VALUES (?, ?, 'del_f1', 'x', 'video', ?, ?)`, workID, creatorID, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(
		`INSERT INTO assets (work_id, kind, path, size_bytes, created_at)
		 VALUES (?, 'video', ?, 64, ?)`, workID, f1, now); err != nil {
		t.Fatal(err)
	}
	req2, _ := http.NewRequest(http.MethodDelete, server.URL+fmt.Sprintf("/api/creators/%d?delete_files=true", creatorID), nil)
	resp, err = client.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete-with-files status = %d", resp.StatusCode)
	}
	if _, err := os.Stat(f1); !os.IsNotExist(err) {
		t.Fatalf("file should be deleted when delete_files=true: %v", err)
	}
}
