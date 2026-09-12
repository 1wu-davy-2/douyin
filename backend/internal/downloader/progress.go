package downloader

// Progress plumbing: in-memory accumulation + a single 1 Hz writer.
//
// The legacy implementation opened a new SQLite connection per 256 KiB chunk
// to write progress - thousands of connections per video. Here the download
// loop only bumps atomic counters; one dedicated goroutine flushes every
// running job at 1 Hz over ONE dedicated *sql.Conn (created in Start) and
// publishes the throttled SSE download.progress event with a sliding-window
// speed estimate. The final byte counts land in the terminal status UPDATE,
// so no state is lost between flush ticks.

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"douyin/backend/internal/events"
)

// tracker is the per-running-job progress state. The download loop writes the
// atomic counters; the flusher reads them and computes speed samples.
type tracker struct {
	jobID      int64
	downloaded atomic.Int64
	total      atomic.Int64
	lastSpeed  atomic.Int64 // most recent speed_bps estimate

	mu      sync.Mutex
	samples []speedSample
}

type speedSample struct {
	at    time.Time
	bytes int64
}

// addTracker registers a tracker for a running job.
func (d *Downloader) addTracker(jobID, total int64) *tracker {
	tr := &tracker{jobID: jobID}
	tr.total.Store(total)
	d.mu.Lock()
	d.trackers[jobID] = tr
	d.mu.Unlock()
	return tr
}

func (d *Downloader) removeTracker(jobID int64) {
	d.mu.Lock()
	delete(d.trackers, jobID)
	d.mu.Unlock()
}

// snapshot returns the current trackers.
func (d *Downloader) snapshotTrackers() []*tracker {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]*tracker, 0, len(d.trackers))
	for _, tr := range d.trackers {
		out = append(out, tr)
	}
	return out
}

// SpeedBps returns the latest speed estimate for a job (0 when not running).
func (d *Downloader) SpeedBps(jobID int64) int64 {
	d.mu.Lock()
	tr, ok := d.trackers[jobID]
	d.mu.Unlock()
	if !ok {
		return 0
	}
	return tr.lastSpeed.Load()
}

// recordSample appends a (now, bytes) sample and prunes the sliding window,
// returning the estimated bytes/second over the window.
func (tr *tracker) recordSample(now time.Time, cur int64) int64 {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.samples = append(tr.samples, speedSample{at: now, bytes: cur})

	cutoff := now.Add(-speedWindow)
	i := 0
	for i < len(tr.samples)-1 && tr.samples[i].at.Before(cutoff) {
		i++
	}
	tr.samples = tr.samples[i:]

	first := tr.samples[0]
	dt := now.Sub(first.at)
	if dt <= 0 || cur < first.bytes {
		return 0
	}
	return int64(float64(cur-first.bytes) / dt.Seconds())
}

// flushLoop ticks at 1 Hz, persists progress for every running job over the
// dedicated connection and publishes download.progress events. On shutdown it
// performs one final flush so "Stop waits for in-flight jobs to land" holds.
func (d *Downloader) flushLoop() {
	defer d.wg.Done()
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-d.baseCtx.Done():
			d.flushOnce(true)
			return
		case <-ticker.C:
			d.flushOnce(false)
		}
	}
}

// flushOnce persists and publishes the progress of all running jobs. final
// forces a write even when the byte counters did not move.
func (d *Downloader) flushOnce(final bool) {
	d.mu.Lock()
	conn := d.conn
	d.mu.Unlock()
	if conn == nil {
		return
	}

	now := time.Now()
	for _, tr := range d.snapshotTrackers() {
		cur := tr.downloaded.Load()
		total := tr.total.Load()
		speed := tr.recordSample(now, cur)
		tr.lastSpeed.Store(speed)

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if _, err := conn.ExecContext(ctx, `
			UPDATE download_jobs SET downloaded_bytes = ?, total_bytes = ? WHERE id = ?`,
			cur, total, tr.jobID); err != nil {
			log.Printf("[downloader] job %d: flush progress: %v", tr.jobID, err)
		}
		cancel()

		if d.bus != nil && (final || cur > 0 || total > 0) {
			d.bus.Publish(events.Event{
				Type: events.TypeDownloadProgress,
				Data: events.DownloadProgress{
					JobID:           tr.jobID,
					DownloadedBytes: cur,
					TotalBytes:      total,
					SpeedBps:        speed,
				},
			})
		}
	}
}
