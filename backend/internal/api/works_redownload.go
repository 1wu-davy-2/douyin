package api

// 典型场景:旧工具把图文作品下载成了 540p 视频,需要转成图集格式。
// POST /api/works/redownload {ids:[...], quality?: "..."} -> 409 当有 downloading

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"douyin/backend/internal/downloader"
)

type worksRedownloadRequest struct {
	IDs     []int64 `json:"ids"`
	Quality string  `json:"quality"`
}

type worksRedownloadResult struct {
	Enqueued    []int64           `json:"enqueued"`
	Skipped     []batchDeleteSkip `json:"skipped"`
	FreedBytes  int64             `json:"freed_bytes"`
}

func (s *Server) handleWorksRedownload(w http.ResponseWriter, r *http.Request) {
	var req worksRedownloadRequest
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
		writeError(w, http.StatusConflict, fmt.Sprintf("%d 个作品正在下载中,请先取消后再重新下载", active))
		return
	}

	res := worksRedownloadResult{Enqueued: []int64{}, Skipped: []batchDeleteSkip{}}
	keep := []int64{}
	for _, wid := range req.IDs {
		if ok := s.purgeWorkDownloads(wid, &res.FreedBytes, &res.Skipped); ok {
			keep = append(keep, wid)
		}
	}
	if len(keep) == 0 {
		writeJSON(w, http.StatusOK, res)
		return
	}
	quality := strings.TrimSpace(req.Quality)
	if quality == "" {
		view, verr := s.deps.Store.View(r.Context())
		if verr != nil {
			writeInternalError(w, verr)
			return
		}
		quality = view.DownloadQuality
	}
	result, err := s.deps.Downloader.EnqueueDetailed(r.Context(), keep, quality)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	for _, id := range result.Created {
		res.Enqueued = append(res.Enqueued, id)
	}
	writeJSON(w, http.StatusOK, res)
}

// purgeWorkDownloads deletes a work's asset files, asset rows and download
// jobs while KEEPING the work row; returns true when the work can be
// re-enqueued.
func (s *Server) purgeWorkDownloads(workID int64, freed *int64, skipped *[]batchDeleteSkip) bool {
	var creatorRoot string
	if err := s.deps.DB.QueryRow(`
		SELECT COALESCE(c.download_root, '')
		FROM works w LEFT JOIN creators c ON c.id = w.creator_id
		WHERE w.id = ?`, workID).Scan(&creatorRoot); err != nil {
		*skipped = append(*skipped, batchDeleteSkip{workID, "load work: " + err.Error()})
		return false
	}

	rows, err := s.deps.DB.Query(`SELECT path, COALESCE(size_bytes, 0) FROM assets WHERE work_id = ?`, workID)
	if err != nil {
		*skipped = append(*skipped, batchDeleteSkip{workID, "query assets: " + err.Error()})
		return false
	}
	type fileRef struct{ path string; size int64 }
	files := []fileRef{}
	for rows.Next() {
		var f fileRef
		if err := rows.Scan(&f.path, &f.size); err != nil {
			rows.Close()
			*skipped = append(*skipped, batchDeleteSkip{workID, "scan asset: " + err.Error()})
			return false
		}
		files = append(files, f)
	}
	rows.Close()

	dirs := map[string]bool{}
	for _, f := range files {
		p := resolveAssetPath(s.deps.Cfg.DataDir, f.path)
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			*skipped = append(*skipped, batchDeleteSkip{workID, "delete file: " + err.Error()})
			return false
		}
		*freed += f.size
		dirs[filepath.Dir(p)] = true
	}
	roots := s.knownDownloadRoots(creatorRoot)
	for dir := range dirs {
		s.cleanupEmptyDirs(dir, roots)
	}

	if _, err := s.deps.DB.Exec(`DELETE FROM assets WHERE work_id = ?`, workID); err != nil {
		*skipped = append(*skipped, batchDeleteSkip{workID, "delete assets rows: " + err.Error()})
		return false
	}
	if _, err := s.deps.DB.Exec(`DELETE FROM download_jobs WHERE work_id = ?`, workID); err != nil {
		*skipped = append(*skipped, batchDeleteSkip{workID, "delete jobs: " + err.Error()})
		return false
	}
	return true
}
