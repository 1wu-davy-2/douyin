package downloader

// Queue operations backing the /api/downloads endpoints and the scanner
// Enqueuer seam. The download_jobs table is the persistent state; every
// transition that activates work also pushes the job id into the dispatch
// channel (workers re-validate the row before claiming).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"

	"douyin/backend/internal/provider"
)

// Quality tiers in ascending order of rank.
var qualityRank = map[string]int{"540p": 1, "720p": 2, "1080p": 3}

// activeStatuses are the job statuses that occupy the queue.
var activeStatuses = []string{StatusQueued, StatusPausedQ, StatusDownloading}

// validQuality reports whether q is a contract quality tier.
func validQuality(q string) bool { return qualityRank[q] > 0 }

// pickVariant selects the variant for the wanted quality: exact match, else
// the closest lower tier, else the highest available. Falls back to the
// highest tier when the wanted quality is unknown to the ladder.
func pickVariant(variants []provider.Variant, want string) *provider.Variant {
	if len(variants) == 0 {
		return nil
	}
	for i := range variants {
		if variants[i].Quality == want {
			return &variants[i]
		}
	}
	wantRank, known := qualityRank[want]
	if !known {
		wantRank = 1 << 30 // above every tier so "closest lower" is the best
	}
	var lower, best *provider.Variant
	for i := range variants {
		v := &variants[i]
		if r := qualityRank[v.Quality]; r > 0 {
			if r < wantRank && (lower == nil || r > qualityRank[lower.Quality]) {
				lower = v
			}
			if best == nil || r > qualityRank[best.Quality] {
				best = v
			}
		}
	}
	if lower != nil {
		return lower
	}
	return best
}

// resolveMedia turns provider media URLs absolute: relative paths (mock mode
// serves "/mockcdn/...") are anchored at BaseURL.
func (d *Downloader) resolveMedia(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "/") && d.baseURL != "" {
		return d.baseURL + raw
	}
	return raw
}

// ----------------------------------------------------------------- enqueue --

// EnqueueSkip explains why a work was not queued.
type EnqueueSkip struct {
	WorkID int64  `json:"work_id"`
	Reason string `json:"reason"`
}

// EnqueueResult is the POST /api/downloads response payload.
type EnqueueResult struct {
	Created []int64       `json:"created"`
	Skipped []EnqueueSkip `json:"skipped"`
}

// Enqueue implements the scanner Enqueuer seam (auto-download after scans).
// Created/skipped counts are logged; the scan flow only needs the error.
func (d *Downloader) Enqueue(ctx context.Context, workIDs []int64, quality string) error {
	res, err := d.EnqueueDetailed(ctx, workIDs, quality)
	if err != nil {
		return err
	}
	log.Printf("[downloader] auto-download: %d job(s) created, %d skipped", len(res.Created), len(res.Skipped))
	return nil
}

// EnqueueDetailed queues the works for download at the given quality (empty
// resolves to the download_quality setting). Skips works that already have a
// succeeded video asset at that quality or an active job for it.
func (d *Downloader) EnqueueDetailed(ctx context.Context, workIDs []int64, quality string) (*EnqueueResult, error) {
	quality = strings.TrimSpace(quality)
	if quality == "" {
		if d.store != nil {
			if view, err := d.store.View(ctx); err == nil && view.DownloadQuality != "" {
				quality = view.DownloadQuality
			}
		}
		if quality == "" {
			quality = "1080p"
		}
	}
	if !validQuality(quality) {
		return nil, fmt.Errorf("%w: invalid quality %q (540p|720p|1080p)", ErrInvalid, quality)
	}

	res := &EnqueueResult{Created: make([]int64, 0, len(workIDs)), Skipped: make([]EnqueueSkip, 0)}
	seen := make(map[int64]bool, len(workIDs))
	for _, workID := range workIDs {
		if workID <= 0 || seen[workID] {
			continue
		}
		seen[workID] = true

		var creatorID int64
		err := d.db.QueryRowContext(ctx, `SELECT creator_id FROM works WHERE id = ?`, workID).Scan(&creatorID)
		if errors.Is(err, sql.ErrNoRows) {
			res.Skipped = append(res.Skipped, EnqueueSkip{WorkID: workID, Reason: "work not found"})
			continue
		}
		if err != nil {
			return res, fmt.Errorf("downloader: load work %d: %w", workID, err)
		}

		// Already downloaded at this quality?
		var done bool
		if err := d.db.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM assets WHERE work_id = ? AND kind = 'video' AND quality = ?)`,
			workID, quality).Scan(&done); err != nil {
			return res, fmt.Errorf("downloader: check assets for work %d: %w", workID, err)
		}
		if done {
			res.Skipped = append(res.Skipped, EnqueueSkip{WorkID: workID, Reason: "already downloaded at " + quality})
			continue
		}

		// Already in the queue at this quality?
		var active bool
		if err := d.db.QueryRowContext(ctx, `
			SELECT EXISTS(SELECT 1 FROM download_jobs WHERE work_id = ? AND quality = ? AND status IN (?, ?, ?))`,
			workID, quality, StatusQueued, StatusPausedQ, StatusDownloading).Scan(&active); err != nil {
			return res, fmt.Errorf("downloader: check active jobs for work %d: %w", workID, err)
		}
		if active {
			res.Skipped = append(res.Skipped, EnqueueSkip{WorkID: workID, Reason: "already queued"})
			continue
		}

		status := StatusQueued
		if d.isPaused() {
			status = StatusPausedQ // enqueued while paused: wait for resume
		}
		ins, err := d.db.ExecContext(ctx, `
			INSERT INTO download_jobs (work_id, creator_id, quality, status, attempts, queued_at)
			VALUES (?, ?, ?, ?, 0, ?)`, workID, creatorID, quality, status, nowRFC3339())
		if err != nil {
			return res, fmt.Errorf("downloader: insert job for work %d: %w", workID, err)
		}
		id, err := ins.LastInsertId()
		if err != nil {
			return res, fmt.Errorf("downloader: job id for work %d: %w", workID, err)
		}
		res.Created = append(res.Created, id)
		if status == StatusQueued {
			d.q.push(id) // paused rows are dispatched by Resume
		}
		d.publishStatus(id, workID, status, nil)
	}
	return res, nil
}

// ----------------------------------------------------------------- job ops --

// Retry requeues one job (allowed from failed/canceled/succeeded; optionally
// switching quality). Active jobs are rejected with ErrConflict.
func (d *Downloader) Retry(ctx context.Context, jobID int64, quality *string) error {
	if quality != nil {
		q := strings.TrimSpace(*quality)
		if !validQuality(q) {
			return fmt.Errorf("%w: invalid quality %q (540p|720p|1080p)", ErrInvalid, q)
		}
		quality = &q
	}
	var workID int64
	err := d.db.QueryRowContext(ctx, `SELECT work_id FROM download_jobs WHERE id = ?`, jobID).Scan(&workID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("downloader: load job %d: %w", jobID, err)
	}

	var q any
	if quality != nil {
		q = *quality
	}
	res, err := d.db.ExecContext(ctx, `
		UPDATE download_jobs SET status = ?, quality = COALESCE(?, quality), error = NULL,
		       finished_at = NULL, downloaded_bytes = 0, total_bytes = 0
		WHERE id = ? AND status IN (?, ?, ?)`,
		StatusQueued, q, jobID, StatusFailed, StatusCanceled, StatusSucceeded)
	if err != nil {
		return fmt.Errorf("downloader: retry job %d: %w", jobID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrConflict // queued/downloading/paused_q
	}
	d.q.push(jobID)
	log.Printf("[downloader] job %d: requeued", jobID)
	d.publishStatus(jobID, workID, StatusQueued, nil)
	return nil
}

// Cancel stops a job. Queued/paused rows are marked canceled directly; a
// running download is aborted through its context (the loop then finalizes
// the row and removes the .part file). Terminal jobs answer ErrConflict.
// The row can move between statuses while we work (a worker claiming the job
// we just decided to cancel), so state is re-read until one branch lands.
func (d *Downloader) Cancel(ctx context.Context, jobID int64) error {
	const maxRounds = 5
	for round := 0; ; round++ {
		var status string
		var workID int64
		err := d.db.QueryRowContext(ctx,
			`SELECT status, work_id FROM download_jobs WHERE id = ?`, jobID).Scan(&status, &workID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("downloader: load job %d: %w", jobID, err)
		}

		switch status {
		case StatusQueued, StatusPausedQ:
			res, uerr := d.db.ExecContext(ctx, `
				UPDATE download_jobs SET status = ?, finished_at = ?, error = 'canceled by user'
				WHERE id = ? AND status IN (?, ?)`, StatusCanceled, nowRFC3339(), jobID, StatusQueued, StatusPausedQ)
			if uerr != nil {
				return fmt.Errorf("downloader: cancel job %d: %w", jobID, uerr)
			}
			if n, _ := res.RowsAffected(); n > 0 {
				log.Printf("[downloader] job %d: canceled while queued", jobID)
				d.publishStatus(jobID, workID, StatusCanceled, strPtr("canceled by user"))
				return nil
			}
			// A worker claimed the job between the read and the update:
			// re-read and handle the downloading case.
		case StatusDownloading:
			if cancel := d.cancelFuncFor(jobID); cancel != nil {
				cancel() // the download loop finalizes the row and removes .part
				return nil
			}
			// Downloading without a live loop (e.g. stale row from a crashed
			// worker): finalize directly.
			res, uerr := d.db.ExecContext(ctx, `
				UPDATE download_jobs SET status = ?, finished_at = ?, error = 'canceled by user'
				WHERE id = ? AND status = ?`, StatusCanceled, nowRFC3339(), jobID, StatusDownloading)
			if uerr != nil {
				return fmt.Errorf("downloader: cancel job %d: %w", jobID, uerr)
			}
			if n, _ := res.RowsAffected(); n > 0 {
				d.publishStatus(jobID, workID, StatusCanceled, strPtr("canceled by user"))
				return nil
			}
		default:
			return ErrConflict
		}
		if round >= maxRounds {
			// The job kept changing state under us (it finished while we
			// canceled): treat as the terminal state it reached.
			return ErrConflict
		}
	}
}

// DeleteJob removes a job record. Downloading jobs are refused (ErrConflict).
func (d *Downloader) DeleteJob(ctx context.Context, jobID int64) error {
	var status string
	err := d.db.QueryRowContext(ctx,
		`SELECT status FROM download_jobs WHERE id = ?`, jobID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("downloader: load job %d: %w", jobID, err)
	}
	if status == StatusDownloading {
		return ErrConflict
	}
	res, err := d.db.ExecContext(ctx, `DELETE FROM download_jobs WHERE id = ?`, jobID)
	if err != nil {
		return fmt.Errorf("downloader: delete job %d: %w", jobID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Batch applies action to the given job ids and returns how many were
// affected. Failures on individual ids are skipped (counted only on success).
func (d *Downloader) Batch(ctx context.Context, action string, ids []int64, quality *string) (int64, error) {
	switch action {
	case "retry", "cancel", "delete":
	default:
		return 0, fmt.Errorf("%w: invalid action %q (retry|cancel|delete)", ErrInvalid, action)
	}
	var affected int64
	for _, id := range ids {
		var err error
		switch action {
		case "retry":
			err = d.Retry(ctx, id, quality)
		case "cancel":
			err = d.Cancel(ctx, id)
		case "delete":
			err = d.DeleteJob(ctx, id)
		}
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrConflict) {
			continue // per-id best effort
		}
		if err != nil {
			return affected, err
		}
		affected++
	}
	return affected, nil
}

// RetryFailed requeues every failed job with attempts < maxAttempts.
func (d *Downloader) RetryFailed(ctx context.Context) (int64, error) {
	ids, err := d.requeueWhere(ctx,
		`SELECT id, work_id FROM download_jobs WHERE status = ? AND attempts < ?`, []any{StatusFailed, maxAttempts},
		`UPDATE download_jobs SET status = ?, error = NULL, finished_at = NULL,
		        downloaded_bytes = 0, total_bytes = 0
		 WHERE id = ? AND status = ?`)
	if err != nil {
		return 0, err
	}
	var affected int64
	for _, id := range ids {
		if d.q.push(id.id) {
			affected++
		}
		d.publishStatus(id.id, id.workID, StatusQueued, nil)
	}
	return affected, nil
}

// ClearCompleted deletes succeeded + canceled job records.
func (d *Downloader) ClearCompleted(ctx context.Context) (int64, error) {
	res, err := d.db.ExecContext(ctx,
		`DELETE FROM download_jobs WHERE status IN (?, ?)`, StatusSucceeded, StatusCanceled)
	if err != nil {
		return 0, fmt.Errorf("downloader: clear completed: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ----------------------------------------------------------------- pause ----

// Pause stops dispatching new jobs (running ones are not interrupted). Queued
// rows flip to paused_q so the job list reflects the state; the flag is
// persisted in the settings table.
func (d *Downloader) Pause(ctx context.Context) error {
	d.setPaused(true)
	if err := d.storePausedFlag(true); err != nil {
		return err
	}
	if _, err := d.db.ExecContext(ctx,
		`UPDATE download_jobs SET status = ? WHERE status = ?`, StatusPausedQ, StatusQueued); err != nil {
		return fmt.Errorf("downloader: pause: %w", err)
	}
	log.Printf("[downloader] queue paused")
	return nil
}

// Resume re-enables dispatch and re-feeds queued rows. With retryFailed,
// failed jobs with attempts < maxAttempts are requeued as well.
func (d *Downloader) Resume(ctx context.Context, retryFailed bool) error {
	d.setPaused(false)
	if err := d.storePausedFlag(false); err != nil {
		return err
	}
	if retryFailed {
		if _, err := d.RetryFailed(ctx); err != nil {
			return err
		}
	}
	if _, err := d.db.ExecContext(ctx,
		`UPDATE download_jobs SET status = ? WHERE status = ?`, StatusQueued, StatusPausedQ); err != nil {
		return fmt.Errorf("downloader: resume: %w", err)
	}
	log.Printf("[downloader] queue resumed")
	d.feedQueued()
	return nil
}

// ----------------------------------------------------------------- summary --

// Summary is the GET /api/downloads/summary payload.
type Summary struct {
	Queued      int64 `json:"queued"`
	Downloading int64 `json:"downloading"`
	Failed      int64 `json:"failed"`
	Succeeded   int64 `json:"succeeded"`
	Canceled    int64 `json:"canceled"`
	Paused      bool  `json:"paused"`
	Concurrency int   `json:"concurrency"`
}

// Summary counts jobs by status (paused_q folds into queued, matching the
// dl_status derivation in the works list).
func (d *Downloader) Summary(ctx context.Context) (Summary, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM download_jobs GROUP BY status`)
	if err != nil {
		return Summary{}, fmt.Errorf("downloader: summary: %w", err)
	}
	defer rows.Close()
	s := Summary{Paused: d.Paused(), Concurrency: d.Concurrency()}
	for rows.Next() {
		var status string
		var n int64
		if err := rows.Scan(&status, &n); err != nil {
			return Summary{}, err
		}
		switch status {
		case StatusQueued, StatusPausedQ:
			s.Queued += n
		case StatusDownloading:
			s.Downloading = n
		case StatusFailed:
			s.Failed = n
		case StatusSucceeded:
			s.Succeeded = n
		case StatusCanceled:
			s.Canceled = n
		}
	}
	return s, rows.Err()
}

// ------------------------------------------------------------- requeue util --

type idWork struct {
	id     int64
	workID int64
}

// requeueWhere selects (id, work_id) rows and updates each back to queued.
func (d *Downloader) requeueWhere(ctx context.Context, selectQ string, args []any, updateQ string) ([]idWork, error) {
	rows, err := d.db.QueryContext(ctx, selectQ, args...)
	if err != nil {
		return nil, fmt.Errorf("downloader: select jobs: %w", err)
	}
	var ids []idWork
	for rows.Next() {
		var r idWork
		if err := rows.Scan(&r.id, &r.workID); err == nil {
			ids = append(ids, r)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, r := range ids {
		if _, err := d.db.ExecContext(ctx, updateQ, StatusQueued, r.id, StatusFailed); err != nil {
			return ids, fmt.Errorf("downloader: requeue job %d: %w", r.id, err)
		}
	}
	return ids, nil
}
