package downloader

// Downloader behaviour tests against an in-process fake CDN (httptest):
//
//   - worker gate: exactly download_concurrency transfers in flight
//   - cancel running -> canceled + .part removed; cancel queued -> immediate
//   - pause holds dispatch, resume continues
//   - retry with a different quality, batch actions, retry-failed ceiling
//   - summary counts (paused_q folds into queued)
//   - startup recovery of rows left downloading by Stop / a crash
//   - progress event throttling (< 10 events for a 2 MiB job)
//   - relative media URLs anchored at BaseURL (the mock-mode chain)
//   - download layout, assets rows, filename conflict sequences
//   - variant fallback ladder and filename sanitizing helpers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"douyin/backend/internal/events"
	"douyin/backend/internal/provider"
)

// The worker gate limits concurrent transfers to download_concurrency: with
// six slow jobs and a limit of three the fake CDN never sees more than three
// simultaneous requests, and every job still finishes.
func TestWorkerPoolConcurrencyLimit(t *testing.T) {
	h := newHarness(t)
	h.setConcurrency(3)
	h.cdn.chunkDelay = time.Millisecond
	h.start()

	creatorID := h.creator("并发博主", "MS4wLjABAAAAconc_test")
	var works []int64
	for i := 1; i <= 6; i++ {
		works = append(works, h.work(creatorID, fmt.Sprintf("conc_%02d", i), fmt.Sprintf("并发作品 %d", i), nil))
	}
	res := h.enqueue(works, "1080p")
	if len(res.Created) != 6 || len(res.Skipped) != 0 {
		t.Fatalf("enqueue = %+v, want 6 created", res)
	}

	for _, id := range res.Created {
		id := id
		waitFor(t, 15*time.Second, func() bool { return h.jobState(id).Status == StatusSucceeded },
			fmt.Sprintf("job %d to succeed", id))
	}
	if peak := h.cdn.peakInflight(); peak != 3 {
		t.Fatalf("peak concurrent CDN transfers = %d, want exactly 3", peak)
	}
}

// Canceling a running download: the job ends canceled and no .part file
// survives.
func TestCancelRunningJob(t *testing.T) {
	h := newHarness(t)
	h.setConcurrency(1)
	h.cdn.size = 2 << 20
	h.cdn.chunkDelay = 2 * time.Millisecond
	h.start()

	creatorID := h.creator("取消博主", "MS4wLjABAAAAcancel_run")
	workID := h.work(creatorID, "cancel_run_1", "取消我", nil)
	jobID := h.enqueue([]int64{workID}, "1080p").Created[0]

	waitFor(t, 10*time.Second, func() bool { return h.jobState(jobID).Status == StatusDownloading },
		"job to start downloading")
	if err := h.dl.Cancel(context.Background(), jobID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	waitFor(t, 10*time.Second, func() bool { return h.jobState(jobID).Status == StatusCanceled },
		"job to become canceled")

	if parts := h.partsUnder(); len(parts) != 0 {
		t.Fatalf(".part files left behind: %v", parts)
	}
	st := h.jobState(jobID)
	if st.Error == nil || !strings.Contains(*st.Error, "canceled") {
		t.Errorf("canceled job error = %v, want canceled-by-user note", st.Error)
	}
}

// Canceling a queued job (worker busy with another) is immediate and the job
// never reaches the CDN.
func TestCancelQueuedJob(t *testing.T) {
	h := newHarness(t)
	h.setConcurrency(1)
	h.cdn.size = 2 << 20
	h.cdn.chunkDelay = 5 * time.Millisecond
	h.start()

	creatorID := h.creator("排队取消", "MS4wLjABAAAAcancel_q")
	workA := h.work(creatorID, "cq_a", "占位任务A", nil)
	workB := h.work(creatorID, "cq_b", "排队任务B", nil)
	jobA := h.enqueue([]int64{workA}, "1080p").Created[0]
	jobB := h.enqueue([]int64{workB}, "1080p").Created[0]

	waitFor(t, 10*time.Second, func() bool { return h.jobState(jobA).Status == StatusDownloading },
		"job A to start downloading")
	if h.jobState(jobB).Status != StatusQueued {
		t.Fatalf("job B status = %s, want queued", h.jobState(jobB).Status)
	}
	if err := h.dl.Cancel(context.Background(), jobB); err != nil {
		t.Fatalf("cancel queued: %v", err)
	}
	if st := h.jobState(jobB).Status; st != StatusCanceled {
		t.Fatalf("job B status after cancel = %s, want canceled", st)
	}
	if n := h.cdn.hitCount("/cdn/cq_b/"); n != 0 {
		t.Fatalf("queued job B reached the CDN %d time(s)", n)
	}
	waitFor(t, 15*time.Second, func() bool { return h.jobState(jobA).Status == StatusSucceeded },
		"job A to succeed")
}

// Pause stops dispatch (queued jobs flip to paused_q and never reach the
// CDN); resume releases them and the summary flag flips back.
func TestPauseResume(t *testing.T) {
	h := newHarness(t)
	h.start()

	if err := h.dl.Pause(context.Background()); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if !h.dl.Paused() {
		t.Fatal("Paused() = false after pause")
	}

	creatorID := h.creator("暂停博主", "MS4wLjABAAAApause")
	var works []int64
	for i := 1; i <= 2; i++ {
		works = append(works, h.work(creatorID, fmt.Sprintf("pause_%d", i), fmt.Sprintf("暂停作品 %d", i), nil))
	}
	res := h.enqueue(works, "1080p")

	time.Sleep(300 * time.Millisecond)
	for _, id := range res.Created {
		if st := h.jobState(id).Status; st != StatusPausedQ {
			t.Fatalf("paused job status = %s, want paused_q", st)
		}
	}
	if n := h.cdn.hitCount("/cdn/pause_"); n != 0 {
		t.Fatalf("paused queue dispatched %d request(s)", n)
	}

	sum, err := h.dl.Summary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !sum.Paused || sum.Queued != 2 {
		t.Fatalf("summary while paused = %+v, want paused with 2 queued", sum)
	}

	if err := h.dl.Resume(context.Background(), false); err != nil {
		t.Fatalf("resume: %v", err)
	}
	for _, id := range res.Created {
		id := id
		waitFor(t, 10*time.Second, func() bool { return h.jobState(id).Status == StatusSucceeded },
			"resumed job to succeed")
	}
	if h.dl.Paused() {
		t.Fatal("Paused() still true after resume")
	}
}

// Retry with a different quality downloads the new tier and keeps the old
// asset (UNIQUE(work_id,kind,quality) permits both).
func TestRetryWithDifferentQuality(t *testing.T) {
	h := newHarness(t)
	h.start()

	creatorID := h.creator("画质博主", "MS4wLjABAAAAquality")
	workID := h.work(creatorID, "quality_1", "画质切换", nil)
	jobID := h.enqueue([]int64{workID}, "720p").Created[0]
	waitFor(t, 10*time.Second, func() bool { return h.jobState(jobID).Status == StatusSucceeded },
		"first download to succeed")

	if q := h.videoAssetQuality(workID); len(q) != 1 || q[0] != "720p" {
		t.Fatalf("assets after first download = %v, want [720p]", q)
	}

	q := "1080p"
	if err := h.dl.Retry(context.Background(), jobID, &q); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if st := h.jobState(jobID).Status; st != StatusQueued {
		t.Fatalf("status after retry = %s, want queued", st)
	}
	waitFor(t, 10*time.Second, func() bool { return h.jobState(jobID).Status == StatusSucceeded },
		"retry to succeed")
	st := h.jobState(jobID)
	if st.Quality != "1080p" || st.Attempts != 2 {
		t.Fatalf("after retry: quality=%s attempts=%d, want 1080p/2", st.Quality, st.Attempts)
	}
	if got := h.videoAssetQuality(workID); len(got) != 2 {
		t.Fatalf("video assets = %v, want 720p and 1080p", got)
	}
}

// Batch applies retry/cancel/delete to many ids and skips per-id conflicts.
// Deterministic: round one lets every job reach a terminal state first; the
// not-retryable state (paused_q) is produced by pausing the queue.
func TestBatchActions(t *testing.T) {
	h := newHarness(t)
	h.setConcurrency(2)
	h.start()
	ctx := context.Background()

	creatorID := h.creator("批处理", "MS4wLjABAAAAbatch")
	w1 := h.work(creatorID, "batch_1", "成功后重试", nil)
	w2 := h.work(creatorID, "batch_2", "也重试", nil)
	w3 := h.work(creatorID, "batch_3", "删掉记录", nil)
	w4 := h.work(creatorID, "batch_4", "失败重试", nil)

	h.prov.setFailItem("batch_4")
	job1 := h.enqueue([]int64{w1, w2, w3}, "540p").Created
	job4 := h.enqueue([]int64{w4}, "540p").Created[0]
	all := append(append([]int64(nil), job1...), job4)
	waitFor(t, 10*time.Second, func() bool {
		for _, id := range all {
			if st := h.jobState(id).Status; st != StatusSucceeded && st != StatusFailed {
				return false
			}
		}
		return true
	}, "round one to reach terminal states")
	if h.jobState(job4).Status != StatusFailed {
		t.Fatalf("job4 = %s, want failed", h.jobState(job4).Status)
	}

	// batch retry over succeeded+failed rows: all retryable -> affected 4.
	h.prov.clearFailItem("batch_4") // the retried job4 must be able to pass
	n, err := h.dl.Batch(ctx, "retry", all, nil)
	if err != nil {
		t.Fatalf("batch retry: %v", err)
	}
	if n != 4 {
		t.Fatalf("batch retry affected = %d, want 4", n)
	}
	waitFor(t, 15*time.Second, func() bool {
		for _, id := range all {
			if h.jobState(id).Status != StatusSucceeded {
				return false
			}
		}
		return true
	}, "retried jobs to succeed again")

	// A paused_q job is not retryable: pause, enqueue, then batch against it.
	if err := h.dl.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	w5 := h.work(creatorID, "batch_5", "暂停中被批处理", nil)
	job5 := h.enqueue([]int64{w5}, "540p").Created[0]
	if st := h.jobState(job5).Status; st != StatusPausedQ {
		t.Fatalf("job5 = %s, want paused_q", st)
	}
	if n, err := h.dl.Batch(ctx, "retry", []int64{job5}, nil); err != nil || n != 0 {
		t.Fatalf("batch retry on paused_q = %d, %v; want 0", n, err)
	}
	if n, err := h.dl.Batch(ctx, "cancel", []int64{job5}, nil); err != nil || n != 1 {
		t.Fatalf("batch cancel = %d, %v; want 1", n, err)
	}
	if st := h.jobState(job5).Status; st != StatusCanceled {
		t.Fatalf("job5 after batch cancel = %s, want canceled", st)
	}

	// batch delete removes canceled + succeeded records.
	n, err = h.dl.Batch(ctx, "delete", []int64{job5, job1[2]}, nil)
	if err != nil || n != 2 {
		t.Fatalf("batch delete = %d, %v; want 2", n, err)
	}
	if err := h.dl.Retry(ctx, job1[2], nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("retry deleted job err = %v, want ErrNotFound", err)
	}
	if _, err := h.dl.Batch(ctx, "purge", []int64{job1[0]}, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid action err = %v, want ErrInvalid", err)
	}
	if err := h.dl.Resume(ctx, false); err != nil {
		t.Fatal(err)
	}
}

// RetryFailed respects the attempts ceiling of 5.
func TestRetryFailedAttemptsCeiling(t *testing.T) {
	h := newHarness(t)
	h.start()
	ctx := context.Background()

	creatorID := h.creator("重试上限", "MS4wLjABAAAAceiling")
	w := h.work(creatorID, "ceiling_1", "一直失败", nil)
	h.prov.fail = true
	jobID := h.enqueue([]int64{w}, "1080p").Created[0]

	for i := 1; i <= 5; i++ {
		waitFor(t, 10*time.Second, func() bool { return h.jobState(jobID).Status == StatusFailed },
			fmt.Sprintf("failure %d", i))
		if i < 5 {
			if n, err := h.dl.RetryFailed(ctx); err != nil || n != 1 {
				t.Fatalf("retry-failed round %d = %d, %v; want 1", i, n, err)
			}
		} else if n, err := h.dl.RetryFailed(ctx); err != nil || n != 0 {
			t.Fatalf("retry-failed at attempts=5 = %d, %v; want 0", n, err)
		}
	}
	if st := h.jobState(jobID).Attempts; st != 5 {
		t.Fatalf("attempts = %d, want 5", st)
	}
}

// Summary counts by status with paused_q folded into queued.
func TestSummaryCounts(t *testing.T) {
	h := newHarness(t)
	h.setConcurrency(2)
	h.start()
	ctx := context.Background()

	creatorID := h.creator("统计博主", "MS4wLjABAAAAsummary")
	wOK := h.work(creatorID, "sum_ok", "成功", nil)
	wFail := h.work(creatorID, "sum_fail", "失败", nil)
	wCancel := h.work(creatorID, "sum_cancel", "取消", nil)

	jobOK := h.enqueue([]int64{wOK}, "540p").Created[0]
	waitFor(t, 10*time.Second, func() bool { return h.jobState(jobOK).Status == StatusSucceeded },
		"job to succeed")

	h.prov.setFailItem("sum_fail")
	jobFail := h.enqueue([]int64{wFail}, "540p").Created[0]
	waitFor(t, 10*time.Second, func() bool { return h.jobState(jobFail).Status == StatusFailed },
		"job to fail")

	jobCancel := h.enqueue([]int64{wCancel}, "540p").Created[0]
	if err := h.dl.Cancel(ctx, jobCancel); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	if err := h.dl.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	wQ1 := h.work(creatorID, "sum_q1", "排队1", nil)
	wQ2 := h.work(creatorID, "sum_q2", "排队2", nil)
	h.enqueue([]int64{wQ1, wQ2}, "540p")

	sum, err := h.dl.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := Summary{Queued: 2, Downloading: 0, Failed: 1, Succeeded: 1, Canceled: 1,
		Paused: true, Concurrency: 2}
	if sum != want {
		t.Fatalf("summary = %+v, want %+v", sum, want)
	}

	if err := h.dl.Resume(ctx, false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 15*time.Second, func() bool {
		s, _ := h.dl.Summary(ctx)
		return s.Queued == 0 && s.Succeeded == 3 && s.Failed == 1 && s.Canceled == 1
	}, "summary to settle after resume")
}

// Stop leaves downloading rows behind on purpose; RecoverStale on a fresh
// Downloader instance (the "restart") requeues them with the marker error and
// they complete afterwards.
func TestRecoverStaleAndShutdownRecovery(t *testing.T) {
	h := newHarness(t)
	h.cdn.size = 2 << 20
	h.cdn.chunkDelay = 10 * time.Millisecond // ~5s per download
	h.start()

	creatorID := h.creator("恢复博主", "MS4wLjABAAAArecover")
	w := h.work(creatorID, "recover_1", "重启续传", nil)
	jobID := h.enqueue([]int64{w}, "1080p").Created[0]
	waitFor(t, 10*time.Second, func() bool { return h.jobState(jobID).Status == StatusDownloading },
		"job to start downloading")

	h.dl.Stop(3 * time.Second)
	if st := h.jobState(jobID).Status; st != StatusDownloading {
		t.Fatalf("status after Stop = %s, want downloading (requeued by next RecoverStale)", st)
	}

	dl2 := New(context.Background(), Deps{
		DB:      h.db,
		Bus:     h.bus,
		Store:   h.store,
		Source:  fakeSource{h.prov},
		DataDir: h.dataDir,
		Mock:    true,
	})
	t.Cleanup(func() { dl2.Stop(3 * time.Second) })
	n, err := dl2.RecoverStale()
	if err != nil || n != 1 {
		t.Fatalf("RecoverStale = %d, %v; want 1", n, err)
	}
	st := h.jobState(jobID)
	if st.Status != StatusQueued || st.Error == nil || *st.Error != "recovered after restart" {
		t.Fatalf("recovered job = %+v", st)
	}
	if err := dl2.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, func() bool { return h.jobState(jobID).Status == StatusSucceeded },
		"recovered job to succeed after restart")

	// A manually inserted stale downloading row is requeued as well.
	res, err := h.db.Exec(`
		INSERT INTO download_jobs (work_id, creator_id, quality, status, attempts, queued_at)
		VALUES (?, ?, '540p', 'downloading', 1, ?)`, w, creatorID, nowRFC3339())
	if err != nil {
		t.Fatal(err)
	}
	stray, _ := res.LastInsertId()
	if _, err := dl2.RecoverStale(); err != nil {
		t.Fatal(err)
	}
	if st := h.jobState(stray).Status; st != StatusQueued {
		t.Fatalf("stray downloading row = %s, want queued", st)
	}
}

// Progress events are throttled: a 2 MiB job produces fewer than 10
// download.progress events (the legacy code persisted per 256 KiB chunk).
func TestProgressEventThrottling(t *testing.T) {
	h := newHarness(t)
	h.setConcurrency(1)
	h.cdn.size = 2 << 20
	h.cdn.chunkDelay = 2 * time.Millisecond
	h.start()

	subCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := h.bus.Subscribe(subCtx)

	creatorID := h.creator("进度博主", "MS4wLjABAAAAthrottle")
	w := h.work(creatorID, "throttle_1", "进度节流", nil)
	jobID := h.enqueue([]int64{w}, "1080p").Created[0]

	progress := 0
	deadline := time.After(15 * time.Second)
	for done := false; !done; {
		select {
		case evt, ok := <-ch:
			if !ok {
				done = true
				break
			}
			switch data := evt.Data.(type) {
			case events.DownloadProgress:
				if data.JobID == jobID {
					progress++
					if data.TotalBytes != int64(2<<20) {
						t.Errorf("progress total = %d, want %d", data.TotalBytes, int64(2<<20))
					}
				}
			case events.DownloadStatus:
				if data.JobID == jobID && (data.Status == StatusSucceeded || data.Status == StatusFailed) {
					done = true
				}
			}
		case <-deadline:
			t.Fatal("job did not finish in time")
		}
	}
	if st := h.jobState(jobID).Status; st != StatusSucceeded {
		t.Fatalf("status = %s (%v)", st, h.jobState(jobID).Error)
	}
	if progress == 0 {
		t.Fatal("no progress events emitted")
	}
	if progress >= 10 {
		t.Fatalf("progress events = %d, want < 10 (throttling broken)", progress)
	}
	t.Logf("progress events for 2MiB job: %d", progress)
}

// Relative media URLs (mock mode) are anchored at BaseURL.
func TestRelativeMediaURLs(t *testing.T) {
	h := newHarness(t)
	h.dl.SetBaseURL(h.cdn.srv.URL) // provider hands out plain "/cdn/..." paths
	h.prov.cdnURL = ""
	h.start()

	creatorID := h.creator("相对地址", "MS4wLjABAAAArelative")
	w := h.work(creatorID, "rel_1", "相对地址作品", nil)
	jobID := h.enqueue([]int64{w}, "720p").Created[0]
	waitFor(t, 10*time.Second, func() bool { return h.jobState(jobID).Status == StatusSucceeded },
		"download with relative URL to succeed")
	if n := h.cdn.hitCount("/cdn/rel_1/"); n == 0 {
		t.Fatal("relative URL was never requested from the fake CDN")
	}
}

// Provider failure during WorkDetail marks the job failed with the cause.
func TestProviderFailureMarksJobFailed(t *testing.T) {
	h := newHarness(t)
	h.prov.fail = true
	h.start()

	creatorID := h.creator("故障博主", "MS4wLjABAAAAfail")
	w := h.work(creatorID, "fail_1", "失败作品", nil)
	jobID := h.enqueue([]int64{w}, "1080p").Created[0]
	waitFor(t, 10*time.Second, func() bool { return h.jobState(jobID).Status == StatusFailed },
		"job to fail")
	st := h.jobState(jobID)
	if st.Error == nil || !strings.Contains(*st.Error, "refresh media") {
		t.Fatalf("failed job error = %v, want refresh-media cause", st.Error)
	}
}

// Enqueue skips already-downloaded qualities, active jobs and unknown works;
// a different quality is still created.
func TestEnqueueDetailedSkips(t *testing.T) {
	h := newHarness(t)
	h.start()
	ctx := context.Background()

	creatorID := h.creator("跳过博主", "MS4wLjABAAAAskip")
	w1 := h.work(creatorID, "skip_1", "已下载", nil)
	w2 := h.work(creatorID, "skip_2", "在队", nil)
	w3 := h.work(creatorID, "skip_3", "新作品", nil)

	job := h.enqueue([]int64{w1}, "720p").Created[0]
	waitFor(t, 10*time.Second, func() bool { return h.jobState(job).Status == StatusSucceeded },
		"seed download to succeed")
	h.enqueue([]int64{w2}, "720p") // stays queued

	res, err := h.dl.EnqueueDetailed(ctx, []int64{w1, w2, w3, 999999}, "720p")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Created) != 1 {
		t.Fatalf("created = %v, want exactly the new work", res.Created)
	}
	if len(res.Skipped) != 3 {
		t.Fatalf("skipped = %+v, want 3 entries", res.Skipped)
	}
	byReason := map[int64]string{}
	for _, s := range res.Skipped {
		byReason[s.WorkID] = s.Reason
	}
	if !strings.Contains(byReason[w1], "already downloaded") {
		t.Errorf("w1 skip reason = %q", byReason[w1])
	}
	if !strings.Contains(byReason[w2], "already queued") {
		t.Errorf("w2 skip reason = %q", byReason[w2])
	}
	if byReason[999999] != "work not found" {
		t.Errorf("unknown skip reason = %q", byReason[999999])
	}

	res2, err := h.dl.EnqueueDetailed(ctx, []int64{w1}, "1080p")
	if err != nil || len(res2.Created) != 1 {
		t.Fatalf("re-enqueue at other quality = %+v, %v", res2, err)
	}
	if _, err := h.dl.EnqueueDetailed(ctx, []int64{w3}, "8k"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid quality err = %v, want ErrInvalid", err)
	}
}

// The downloader satisfies the scanner Enqueuer seam.
func TestImplementsScannerEnqueuer(t *testing.T) {
	h := newHarness(t)
	var _ interface {
		Enqueue(ctx context.Context, workIDs []int64, quality string) error
	} = h.dl
}

// Files land in <data>/downloads/{nickname}_{sec_uid}/{collections|singles}
// with video/cover/metadata assets recorded as relative forward-slash paths.
func TestDownloadLayoutAndAssets(t *testing.T) {
	h := newHarness(t)
	h.start()

	creatorID := h.creator("布局博主", "MS4wLjABAAAAlayout")
	wSingle := h.work(creatorID, "layout_single", `单发/作品: 试题?`, nil)
	res, err := h.db.Exec(
		`INSERT INTO collections (creator_id, mix_id, name, created_at) VALUES (?, 'mix_x', '合集X', ?)`,
		creatorID, nowRFC3339())
	if err != nil {
		t.Fatal(err)
	}
	collID, _ := res.LastInsertId()
	wColl := h.work(creatorID, "layout_coll", "合集作品", collID)

	created := h.enqueue([]int64{wSingle, wColl}, "1080p").Created
	for _, id := range created {
		id := id
		waitFor(t, 10*time.Second, func() bool { return h.jobState(id).Status == StatusSucceeded },
			"layout job to succeed")
	}

	singlesDir := filepath.Join(h.dataDir, "downloads", "布局博主_MS4wLjABAAAAlayout", "singles")
	collDir := filepath.Join(h.dataDir, "downloads", "布局博主_MS4wLjABAAAAlayout", "collections")
	for _, dir := range []string{singlesDir, collDir} {
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			t.Fatalf("directory %s missing: %v", dir, err)
		}
	}
	entries, _ := os.ReadDir(singlesDir)
	if len(entries) != 3 {
		t.Fatalf("singles dir has %d entries, want video+cover+metadata", len(entries))
	}
	for _, e := range entries {
		if strings.ContainsAny(e.Name(), `/:*?"<>|`) {
			t.Errorf("unsafe filename %q", e.Name())
		}
	}

	var videoPath, metaPath string
	var coverOK bool
	if err := h.db.QueryRow(
		`SELECT path FROM assets WHERE work_id = ? AND kind = 'video'`, wSingle).Scan(&videoPath); err != nil {
		t.Fatalf("video asset: %v", err)
	}
	if err := h.db.QueryRow(
		`SELECT path FROM assets WHERE work_id = ? AND kind = 'metadata'`, wSingle).Scan(&metaPath); err != nil {
		t.Fatalf("metadata asset: %v", err)
	}
	if err := h.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM assets WHERE work_id = ? AND kind = 'cover')`, wSingle).Scan(&coverOK); err != nil {
		t.Fatal(err)
	}
	if !coverOK {
		t.Fatal("cover asset missing")
	}
	if !strings.HasPrefix(videoPath, "downloads/") || strings.Contains(videoPath, `\`) {
		t.Errorf("video asset path not data-relative/forward-slash: %q", videoPath)
	}
	if _, err := os.Stat(filepath.Join(h.dataDir, filepath.FromSlash(videoPath))); err != nil {
		t.Errorf("video file missing at resolved path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.dataDir, filepath.FromSlash(metaPath))); err != nil {
		t.Errorf("metadata file missing: %v", err)
	}
}

// Filename conflicts get -(2) sequence numbers.
func TestFilenameConflictSequence(t *testing.T) {
	h := newHarness(t)
	h.start()

	creatorID := h.creator("重名博主", "MS4wLjABAAAAconflict")
	w1 := h.work(creatorID, "conf_1", "同名作品", nil)
	w2 := h.work(creatorID, "conf_2", "同名作品", nil)

	created := h.enqueue([]int64{w1, w2}, "540p").Created
	for _, id := range created {
		id := id
		waitFor(t, 10*time.Second, func() bool { return h.jobState(id).Status == StatusSucceeded },
			"conflict job to succeed")
	}
	dir := filepath.Join(h.dataDir, "downloads", "重名博主_MS4wLjABAAAAconflict", "singles")
	if _, err := os.Stat(filepath.Join(dir, "同名作品.mp4")); err != nil {
		t.Errorf("first file missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "同名作品-(2).mp4")); err != nil {
		t.Errorf("conflict-sequence file missing: %v", err)
	}
}

// Unit tests for the variant ladder.
func TestPickVariant(t *testing.T) {
	v540 := provider.Variant{Quality: "540p", SizeBytes: 1}
	v720 := provider.Variant{Quality: "720p", SizeBytes: 2}
	v1080 := provider.Variant{Quality: "1080p", SizeBytes: 3}
	ladder := []provider.Variant{v1080, v720, v540}

	if got := pickVariant(ladder, "720p"); got.Quality != "720p" {
		t.Errorf("exact = %s, want 720p", got.Quality)
	}
	if got := pickVariant(ladder, "1080p"); got.Quality != "1080p" {
		t.Errorf("exact top = %s", got.Quality)
	}
	if got := pickVariant([]provider.Variant{v1080, v540}, "720p"); got.Quality != "540p" {
		t.Errorf("closest lower = %s, want 540p", got.Quality)
	}
	if got := pickVariant([]provider.Variant{v1080}, "540p"); got.Quality != "1080p" {
		t.Errorf("highest fallback = %s, want 1080p", got.Quality)
	}
	if got := pickVariant([]provider.Variant{v540, v720}, "2160p"); got.Quality != "720p" {
		t.Errorf("unknown quality = %s, want 720p", got.Quality)
	}
	if got := pickVariant(nil, "720p"); got != nil {
		t.Errorf("empty ladder = %v, want nil", got)
	}
}

// Unit tests for the Windows-safe filename sanitizer.
func TestSafeName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"正常的标题", "正常的标题"},
		{`a<b>c:d"e/f\g|h|i?j*k`, "a_b_c_d_e_f_g_h_i_j_k"},
		{"尾部的点...", "尾部的点"},
		{"  空格  ", "空格"},
		{"CON", "_CON"},
		{"", "untitled"},
		{strings.Repeat("长", 100), strings.Repeat("长", 80)},
	}
	for _, c := range cases {
		if got := safeName(c.in, 80); got != c.want {
			t.Errorf("safeName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
