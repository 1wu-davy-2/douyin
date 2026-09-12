package downloader

// Contract v1.3 root resolution: every job's target root resolves as
// creators.download_root -> settings.download_root -> <data_dir>/downloads,
// and every new asset row stores an absolute forward-slash path under that
// root (video, cover and metadata alike).

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"douyin/backend/internal/settings"
)

func TestDownloadRootResolutionPriority(t *testing.T) {
	h := newHarness(t)
	h.start()

	// 1. No settings at all: the default root <data_dir>/downloads applies
	//    and the stored asset path is absolute with forward slashes.
	cOverride := h.creator("优先博主", "MS4wLjABAAAAroot_ovr")
	wDefault := h.work(cOverride, "root_def_1", "默认根作品", nil)
	jobID := h.enqueue([]int64{wDefault}, "1080p").Created[0]
	waitFor(t, 15*time.Second, func() bool { return h.jobState(jobID).Status == StatusSucceeded },
		"default-root job to succeed")
	var got string
	if err := h.db.QueryRow(
		`SELECT path FROM assets WHERE work_id = ? AND kind = 'video'`, wDefault).Scan(&got); err != nil {
		t.Fatal(err)
	}
	wantPrefix := filepath.ToSlash(filepath.Join(h.dataDir, "downloads", "优先博主_MS4wLjABAAAAroot_ovr", "singles")) + "/"
	if !strings.HasPrefix(got, wantPrefix) || strings.Contains(got, `\`) {
		t.Fatalf("default-root asset path = %q, want absolute prefix %q", got, wantPrefix)
	}
	if err := h.db.QueryRow(
		`SELECT 1 FROM assets WHERE work_id = ? AND kind = 'metadata' AND path LIKE ?`,
		wDefault, wantPrefix+"%").Scan(new(any)); err != nil {
		t.Fatalf("metadata asset not absolute under the root: %v", err)
	}

	// 2. Global root + creator override: the override wins.
	globalRoot := filepath.Join(h.t.TempDir(), "global-root")
	if _, err := h.store.Apply(context.Background(), settings.Patch{DownloadRoot: &globalRoot}); err != nil {
		t.Fatalf("set global download_root: %v", err)
	}
	overrideRoot := filepath.Join(h.t.TempDir(), "creator-root")
	if _, err := h.db.Exec(`UPDATE creators SET download_root = ? WHERE id = ?`, overrideRoot, cOverride); err != nil {
		t.Fatalf("set creator download_root: %v", err)
	}
	wOverride := h.work(cOverride, "root_ovr_1", "覆盖根作品", nil)
	cGlobal := h.creator("全局博主", "MS4wLjABAAAAroot_glob")
	wGlobal := h.work(cGlobal, "root_glob_1", "全局根作品", nil)

	created := h.enqueue([]int64{wOverride, wGlobal}, "1080p").Created
	if len(created) != 2 {
		t.Fatalf("enqueue = %v, want 2 jobs", created)
	}
	for _, id := range created {
		id := id
		waitFor(t, 15*time.Second, func() bool { return h.jobState(id).Status == StatusSucceeded },
			"root job to succeed")
	}

	var pathOverride, pathGlobal string
	if err := h.db.QueryRow(
		`SELECT path FROM assets WHERE work_id = ? AND kind = 'video'`, wOverride).Scan(&pathOverride); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRow(
		`SELECT path FROM assets WHERE work_id = ? AND kind = 'video'`, wGlobal).Scan(&pathGlobal); err != nil {
		t.Fatal(err)
	}
	if want := filepath.ToSlash(overrideRoot) + "/"; !strings.HasPrefix(pathOverride, want) {
		t.Fatalf("creator-override asset path = %q, want prefix %q (creator root must win)", pathOverride, want)
	}
	if want := filepath.ToSlash(globalRoot) + "/"; !strings.HasPrefix(pathGlobal, want) {
		t.Fatalf("global asset path = %q, want prefix %q (no override falls back to the global root)", pathGlobal, want)
	}

	// 3. Clearing the override (NULL) falls back to the global root.
	if _, err := h.db.Exec(`UPDATE creators SET download_root = NULL WHERE id = ?`, cOverride); err != nil {
		t.Fatalf("clear creator download_root: %v", err)
	}
	wFallback := h.work(cOverride, "root_ovr_2", "清空覆盖作品", nil)
	jobID = h.enqueue([]int64{wFallback}, "1080p").Created[0]
	waitFor(t, 15*time.Second, func() bool { return h.jobState(jobID).Status == StatusSucceeded },
		"fallback job to succeed")
	var pathFallback string
	if err := h.db.QueryRow(
		`SELECT path FROM assets WHERE work_id = ? AND kind = 'video'`, wFallback).Scan(&pathFallback); err != nil {
		t.Fatal(err)
	}
	if want := filepath.ToSlash(globalRoot) + "/"; !strings.HasPrefix(pathFallback, want) {
		t.Fatalf("cleared-override asset path = %q, want prefix %q (NULL follows the global root)", pathFallback, want)
	}
}
