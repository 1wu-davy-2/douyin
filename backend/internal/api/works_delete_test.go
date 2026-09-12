package api

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBatchDeleteWorks(t *testing.T) {
	server, _, database, cfg := newTestServerWithCfg(t)
	client := setupAdmin(t, server)

	now := time.Now().UTC().Format(time.RFC3339)
	res, err := database.Exec(
		`INSERT INTO creators (sec_uid, nickname, profile_url, created_at)
		 VALUES ('MS4wLjABAAAAapi_del', '删除博主', 'u', ?)`, now)
	if err != nil {
		t.Fatal(err)
	}
	creatorID, _ := res.LastInsertId()

	// 临时下载根:放在服务数据目录的 downloads 下(受管区域内),一个文件
	// 存在、一个缺失(按已删处理)。
	absRoot, err := filepath.Abs(filepath.Join(cfg.DataDir, "downloads"))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(absRoot, "delcreator", "singles")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f1 := filepath.Join(dir, "a.mp4")
	if err := os.WriteFile(f1, make([]byte, 100), 0o644); err != nil {
		t.Fatal(err)
	}

	insert := func(itemID, title string) int64 {
		r, err := database.Exec(
			`INSERT INTO works (creator_id, item_id, title, type, created_at, updated_at)
			 VALUES (?, ?, ?, 'video', ?, ?)`, creatorID, itemID, title, now, now)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := r.LastInsertId()
		return id
	}
	wFile := insert("del_file", "有文件")
	wGone := insert("del_gone", "文件缺失")
	wBusy := insert("del_busy", "下载中")

	seedAsset := func(workID int64, p string, size int64) {
		if _, err := database.Exec(
			`INSERT INTO assets (work_id, kind, path, size_bytes, created_at)
			 VALUES (?, 'video', ?, ?, ?)`, workID, p, size, now); err != nil {
			t.Fatal(err)
		}
	}
	seedAsset(wFile, f1, 100)
	seedAsset(wGone, filepath.Join(dir, "gone.mp4"), 42)

	seedJob := func(workID int64, status string) {
		if _, err := database.Exec(
			`INSERT INTO download_jobs (work_id, creator_id, quality, status, attempts, queued_at)
			 VALUES (?, ?, '1080p', ?, 1, ?)`, workID, creatorID, status, now); err != nil {
			t.Fatal(err)
		}
	}
	seedJob(wFile, "succeeded")
	seedJob(wGone, "succeeded")
	seedJob(wBusy, "downloading")

	// 1. downloading -> 409
	resp, err := client.Post(server.URL+"/api/works/batch-delete",
		"application/json", strings.NewReader(fmt.Sprintf(`{"ids":[%d]}`, wBusy)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("busy delete status = %d, want 409", resp.StatusCode)
	}

	// 2. 正常删除:文件存在 + 文件缺失
	resp, err = client.Post(server.URL+"/api/works/batch-delete",
		"application/json", strings.NewReader(fmt.Sprintf(`{"ids":[%d,%d]}`, wFile, wGone)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d", resp.StatusCode)
	}

	if _, err := os.Stat(f1); !os.IsNotExist(err) {
		t.Fatalf("asset file still on disk")
	}
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM works WHERE id IN (?,?)`, wFile, wGone).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("works rows remain: %d", n)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM download_jobs WHERE work_id IN (?,?)`, wFile, wGone).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("job rows remain: %d", n)
	}
	// 下载中的作品保留
	if err := database.QueryRow(`SELECT COUNT(*) FROM works WHERE id = ?`, wBusy).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("busy work was deleted")
	}
	// 空目录被清理(singles 目录删除)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("empty dir not cleaned: %v", err)
	}
}
