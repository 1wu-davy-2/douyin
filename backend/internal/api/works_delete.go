package api

// 批量删除作品(含已下载文件)。POST /api/works/batch-delete {ids:[...]}
// -> {deleted:[...], freed_bytes, skipped:[{work_id, reason}]}
//
// 语义:
//   - 任一作品存在 status='downloading' 的任务 → 整体 409(文件被占用)
//   - 排队中/暂停的任务随作品一并删除
//   - 资产文件先删(缺失按已删处理),全部成功才删库;任何文件删失败则该作品跳过并保留记录
//   - 清理因删除而变空的目录(向上到该作品下载根为止;根本身保留)

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"douyin/backend/internal/downloader"
)

type batchDeleteWorksRequest struct {
	IDs []int64 `json:"ids"`
}

type batchDeleteSkip struct {
	WorkID int64  `json:"work_id"`
	Reason string `json:"reason"`
}

type batchDeleteWorksResult struct {
	Deleted    []int64           `json:"deleted"`
	FreedBytes int64             `json:"freed_bytes"`
	Skipped    []batchDeleteSkip `json:"skipped"`
}

func (s *Server) handleBatchDeleteWorks(w http.ResponseWriter, r *http.Request) {
	var req batchDeleteWorksRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	if len(req.IDs) == 0 {
		writeError(w, http.StatusBadRequest, "ids is required")
		return
	}
	if len(req.IDs) > 1000 {
		writeError(w, http.StatusBadRequest, "too many ids (max 1000)")
		return
	}

	// 正在下载中的作品拒绝删除(文件被写入手柄占用)。
	args := idsArgs(req.IDs)
	var active int
	if err := s.deps.DB.QueryRow(`
		SELECT COUNT(*) FROM download_jobs
		WHERE status = ? AND work_id IN (`+placeholders(len(req.IDs))+`)`,
		append([]any{downloader.StatusDownloading}, args...)...).Scan(&active); err != nil {
		writeInternalError(w, err)
		return
	}
	if active > 0 {
		writeError(w, http.StatusConflict, fmt.Sprintf("%d 个作品正在下载中,请先取消后再删除", active))
		return
	}

	res := batchDeleteWorksResult{Deleted: []int64{}, Skipped: []batchDeleteSkip{}}
	for _, wid := range req.IDs {
		if done := s.deleteOneWork(wid, &res); done {
			res.Deleted = append(res.Deleted, wid)
		}
	}
	writeJSON(w, http.StatusOK, res)
}

// deleteOneWork removes one work's files then rows; returns true when deleted.
func (s *Server) deleteOneWork(workID int64, res *batchDeleteWorksResult) bool {
	var creatorRoot string
	err := s.deps.DB.QueryRow(`
		SELECT COALESCE(c.download_root, '')
		FROM works w LEFT JOIN creators c ON c.id = w.creator_id
		WHERE w.id = ?`, workID).Scan(&creatorRoot)
	if err != nil {
		res.Skipped = append(res.Skipped, batchDeleteSkip{workID, "load work: " + err.Error()})
		return false
	}

	rows, err := s.deps.DB.Query(`SELECT kind, path, COALESCE(size_bytes, 0) FROM assets WHERE work_id = ?`, workID)
	if err != nil {
		res.Skipped = append(res.Skipped, batchDeleteSkip{workID, "query assets: " + err.Error()})
		return false
	}
	type asset struct {
		kind, path string
		size       int64
	}
	assets := []asset{}
	for rows.Next() {
		var a asset
		if err := rows.Scan(&a.kind, &a.path, &a.size); err != nil {
			rows.Close()
			res.Skipped = append(res.Skipped, batchDeleteSkip{workID, "scan asset: " + err.Error()})
			return false
		}
		assets = append(assets, a)
	}
	rows.Close()

	// 删文件(相对路径为历史遗留,按 data_dir 回退解析)。
	deletedDirs := map[string]bool{}
	for _, a := range assets {
		p := resolveAssetPath(s.deps.Cfg.DataDir, a.path)
		if err := os.Remove(p); err != nil {
			if os.IsNotExist(err) {
				continue // 已不在盘上
			}
			res.Skipped = append(res.Skipped, batchDeleteSkip{workID, "delete file: " + err.Error()})
			return false
		}
		res.FreedBytes += a.size
		deletedDirs[filepath.Dir(p)] = true
	}

	// 清理因删除而变空的目录(向上到该作品生效的下载根为止)。
	roots := s.knownDownloadRoots(creatorRoot)
	for dir := range deletedDirs {
		s.cleanupEmptyDirs(dir, roots)
	}

	// 删库(assets -> jobs -> works)。
	if _, err := s.deps.DB.Exec(`DELETE FROM assets WHERE work_id = ?`, workID); err != nil {
		res.Skipped = append(res.Skipped, batchDeleteSkip{workID, "delete assets rows: " + err.Error()})
		return false
	}
	if _, err := s.deps.DB.Exec(`DELETE FROM download_jobs WHERE work_id = ?`, workID); err != nil {
		res.Skipped = append(res.Skipped, batchDeleteSkip{workID, "delete jobs: " + err.Error()})
		return false
	}
	if _, err := s.deps.DB.Exec(`DELETE FROM works WHERE id = ?`, workID); err != nil {
		res.Skipped = append(res.Skipped, batchDeleteSkip{workID, "delete work: " + err.Error()})
		return false
	}
	log.Printf("[works] deleted work %d (%d assets)", workID, len(assets))
	return true
}

// knownDownloadRoots lists the candidate roots for a creator: its own override,
// the global root, the default <data_dir>/downloads, and data_dir itself
// (historical relative rows). Used as cleanup boundaries.
func (s *Server) knownDownloadRoots(creatorRoot string) []string {
	roots := []string{}
	if creatorRoot != "" {
		roots = append(roots, creatorRoot)
	}
	if s.deps.Store != nil {
		if global := s.deps.Store.DownloadRoot(context.Background()); global != "" {
			roots = append(roots, global)
		}
		roots = append(roots, s.deps.Store.DefaultDownloadsRoot())
	}
	if dataDir, err := filepath.Abs(s.deps.Cfg.DataDir); err == nil {
		roots = append(roots, dataDir)
	}
	return roots
}

// cleanupEmptyDirs removes now-empty directories from dir upward, walking only
// INSIDE the managed roots (roots themselves are kept) and stopping at the
// first non-empty directory.
func (s *Server) cleanupEmptyDirs(dir string, roots []string) {
	cleanRoots := make([]string, 0, len(roots))
	for _, root := range roots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		cleanRoots = append(cleanRoots, strings.ToLower(strings.TrimSuffix(filepath.ToSlash(filepath.Clean(root)), "/"))+"/")
	}
	cur := strings.ToLower(strings.TrimSuffix(filepath.ToSlash(filepath.Clean(dir)), "/")) + "/"
	for {
		inside := false
		for _, rc := range cleanRoots {
			if strings.HasPrefix(cur, rc) {
				if strings.EqualFold(strings.TrimSuffix(cur, "/"), strings.TrimSuffix(rc, "/")) {
					return // 到达根本身,保留
				}
				inside = true
				break
			}
		}
		if !inside {
			return // 已离开受管区域
		}
		dirPath := filepath.FromSlash(strings.TrimSuffix(cur, "/"))
		entries, err := os.ReadDir(dirPath)
		if err != nil || len(entries) > 0 {
			return
		}
		if err := os.Remove(dirPath); err != nil {
			return
		}
		cur = filepath.ToSlash(filepath.Dir(strings.TrimSuffix(cur, "/"))) + "/"
	}
}

func placeholders(n int) string {
	if n <= 0 {
		return "NULL"
	}
	p := make([]string, n)
	for i := range p {
		p[i] = "?"
	}
	return strings.Join(p, ",")
}

func idsArgs(ids []int64) []any {
	out := make([]any, len(ids))
	for i, v := range ids {
		out[i] = v
	}
	return out
}
