package downloader

// Single-job execution pipeline:
//
//	claim (queued -> downloading, attempts+1)
//	  -> provider.WorkDetail refresh (90s budget; failure -> failed)
//	  -> video works:                         image works (stage 9):
//	    variant pick (quality ladder)           gallery aggregation directory
//	    stream title.mp4                        0001.jpg.. 000N.jpg (+ live0001.mp4..)
//	  -> cover (best effort; failure only logged)
//	  -> metadata json
//	  -> assets upsert (video|image / cover / metadata)
//	  -> succeeded
//
// Cancellation: a user cancel (Cancel -> job ctx canceled) ends the transfer,
// removes the .part file and marks the job canceled. A shutdown cancel
// (baseCtx canceled) leaves the row as downloading so RecoverStale requeues
// it on the next start.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"douyin/backend/internal/db"
	"douyin/backend/internal/provider"
)

// jobRow is the joined job+work+creator snapshot a worker runs against.
type jobRow struct {
	ID           int64
	WorkID       int64
	CreatorID    int64
	Status       string
	Quality      string
	Attempts     int
	ItemID       string
	Title        string
	WorkType     string
	Nickname     string
	SecUID       string
	CollectionID sql.NullInt64
}

const selectJob = `
	SELECT j.id, j.work_id, j.creator_id, j.status, j.quality, j.attempts,
	       w.item_id, w.title, w.type, c.nickname, c.sec_uid, w.collection_id
	FROM download_jobs j
	JOIN works w    ON w.id = j.work_id
	LEFT JOIN creators c ON c.id = w.creator_id
	WHERE j.id = ?`

func scanJob(row *sql.Row) (*jobRow, error) {
	var j jobRow
	var nickname, secUID sql.NullString
	err := row.Scan(&j.ID, &j.WorkID, &j.CreatorID, &j.Status, &j.Quality, &j.Attempts,
		&j.ItemID, &j.Title, &j.WorkType, &nickname, &secUID, &j.CollectionID)
	if err != nil {
		return nil, err
	}
	j.Nickname = nickname.String
	j.SecUID = secUID.String
	return &j, nil
}

// runJob claims and executes one job. All early returns must release the gate
// slot (deferred after a successful acquire).
func (d *Downloader) runJob(jobID int64) {
	ctx, cancel := context.WithCancel(d.baseCtx)
	if !d.registerCancel(jobID, cancel) {
		cancel()
		return
	}
	defer func() {
		d.unregisterCancel(jobID)
		cancel()
	}()

	if !d.gate.acquire(ctx) {
		return // shutdown while waiting for a slot
	}
	defer d.gate.release()

	job, err := scanJob(d.db.QueryRowContext(ctx, selectJob, jobID))
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("[downloader] job %d: load: %v", jobID, err)
		}
		return // deleted (or vanished) while waiting in the channel
	}
	if job.Status != StatusQueued || d.isPaused() {
		return // canceled/paused between channel and claim
	}

	now := nowRFC3339()
	res, err := d.db.ExecContext(ctx, `
		UPDATE download_jobs SET status = ?, attempts = attempts + 1, started_at = ?,
		       downloaded_bytes = 0, total_bytes = 0, error = NULL, finished_at = NULL
		WHERE id = ? AND status = ?`,
		StatusDownloading, now, jobID, StatusQueued)
	if err != nil {
		log.Printf("[downloader] job %d: claim: %v", jobID, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return // lost the claim race (canceled concurrently)
	}
	d.publishStatus(jobID, job.WorkID, StatusDownloading, nil)
	d.process(ctx, job)
}

// process runs the claimed job to a terminal state, dispatching on the work
// type (stage 9: image/gallery works aggregate into a title directory).
func (d *Downloader) process(ctx context.Context, job *jobRow) {
	// 1. Refresh media addresses (90s budget).
	prov := d.src.Resolve(ctx)
	dctx, dcancel := context.WithTimeout(ctx, detailTimeout)
	detail, err := prov.WorkDetail(dctx, job.ItemID)
	dcancel()
	if err != nil {
		if ctx.Err() != nil { // canceled while refreshing
			d.aborted(ctx, job)
			return
		}
		d.failJob(ctx, job, "refresh media: "+err.Error())
		return
	}

	// The fresh detail type wins; the scanned works.type is the fallback for
	// providers that predate the additive field.
	workType := detail.Type
	if workType == "" {
		workType = job.WorkType
	}
	if workType == provider.TypeImage {
		d.processImageWork(ctx, job, detail)
		return
	}
	d.processVideoWork(ctx, job, detail)
}

// processVideoWork downloads one variant of a video work to
// <data>/downloads/{creator}/{collections|singles}/title.mp4 and records
// video/cover/metadata assets.
func (d *Downloader) processVideoWork(ctx context.Context, job *jobRow, detail *provider.WorkDetail) {
	// 2. Pick the variant for the requested quality.
	variant := pickVariant(detail.Variants, job.Quality)
	if variant == nil {
		d.failJob(ctx, job, "no downloadable variant returned by provider")
		return
	}

	// 3. Target directory: <data_dir>/downloads/{nickname}_{sec_uid}/{collections|singles}/
	dir := job.directory(d.dataDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		d.failJob(ctx, job, "create directory: "+err.Error())
		return
	}
	name := safeName(job.Title, maxTitleLength) + ".mp4"

	// 4. Stream the video (unique .part, then rename), progress in memory only.
	// Try every CDN candidate in order: douyin nodes reject a subset of
	// requests with 403, alternates usually succeed (old-tool behavior).
	tr := d.addTracker(job.ID, variant.SizeBytes)
	var written int64
	var target string
	var lastErr error
	for _, candidate := range variant.Candidates() {
		written, target, lastErr = d.downloadVideo(ctx, job.ID, candidate, dir, name, tr)
		if lastErr == nil {
			break
		}
		if ctx.Err() != nil { // user cancel or shutdown
			break
		}
		log.Printf("[downloader] job %d: candidate failed (%v), trying next", job.ID, lastErr)
		tr.downloaded.Store(0) // restart progress for the next candidate
	}
	if lastErr != nil {
		d.removeTracker(job.ID)
		if ctx.Err() != nil { // user cancel or shutdown
			d.aborted(ctx, job)
			return
		}
		d.failJob(ctx, job, "download: "+lastErr.Error())
		return
	}

	// 5. Cover (optional; failure only logged).
	coverPath, coverSize := d.fetchCover(ctx, dir, strings.TrimSuffix(filepath.Base(target), ".mp4"), detail.CoverURL)

	// 6. metadata.json (normalized WorkDetail JSON).
	metaPath, err := writeMetadata(dir, target, detail, job, variant)
	if err != nil {
		d.removeTracker(job.ID)
		d.failJob(ctx, job, "write metadata: "+err.Error())
		return
	}

	// 7. Asset rows (video upsert by (work_id,kind,quality); cover/metadata
	//    have NULL quality where SQLite unique conflicts never match, so they
	//    are delete+insert).
	if err := d.upsertAssets(ctx, job, assetFiles{
		video:     target,
		videoSize: written,
		quality:   variant.Quality,
		cover:     coverPath,
		coverSize: coverSize,
		metadata:  metaPath,
	}); err != nil {
		d.removeTracker(job.ID)
		d.failJob(ctx, job, "record assets: "+err.Error())
		return
	}

	// 8. Succeeded.
	d.removeTracker(job.ID)
	finished := nowRFC3339()
	if _, err := d.db.ExecContext(ctx, `
		UPDATE download_jobs SET status = ?, finished_at = ?, quality = ?,
		       total_bytes = ?, downloaded_bytes = ?
		WHERE id = ?`, StatusSucceeded, finished, variant.Quality, written, written, job.ID); err != nil {
		log.Printf("[downloader] job %d: finalize succeeded: %v", job.ID, err)
	}
	log.Printf("[downloader] job %d: succeeded %s (%d bytes, %s)", job.ID, filepath.Base(target), written, variant.Quality)
	d.publishStatus(job.ID, job.WorkID, StatusSucceeded, nil)
}

// processImageWork downloads an image (gallery) work into the aggregated
// title directory
//
//	<data>/downloads/{creator}/{collections|singles}/{safeName(title)}/
//
// as 0001.jpg..000N.jpg (extension inferred from the URL) plus optional
// live0001.mp4 live segments, a fixed cover.jpg and metadata.json. Assets:
// kind=image with 4-digit sequence quality, live segments kind=video with
// "live"+sequence quality. Progress is file-based (completed files / total
// files in the tracker; bytes accumulate for the job's final totals).
func (d *Downloader) processImageWork(ctx context.Context, job *jobRow, detail *provider.WorkDetail) {
	if len(detail.Images) == 0 {
		d.failJob(ctx, job, "image work returned no image addresses")
		return
	}

	// Aggregated gallery directory (one level deeper than video works).
	// 无标题作品用 item_id 兜底,避免全部挤进 "untitled" 互相叠加序号。
	dir := filepath.Join(job.collectionBase(d.dataDir), safeName(untitledFallback(job.Title, job.ItemID), maxTitleLength))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		d.failJob(ctx, job, "create directory: "+err.Error())
		return
	}

	totalFiles := len(detail.Images) + len(detail.LiveVideos)
	tr := d.addTracker(job.ID, int64(totalFiles))

	// media collects every produced file for the asset upsert.
	media := make([]galleryAsset, 0, totalFiles)
	var totalBytes int64
	done := 0

	// Gallery images in original order -> 0001.. with URL-inferred extension.
	// Every CDN candidate is tried in order before the image counts as failed.
	for i, img := range detail.Images {
		if ctx.Err() != nil {
			d.removeTracker(job.ID)
			d.aborted(ctx, job)
			return
		}
		name := fmt.Sprintf("%04d%s", i+1, extFromURL(img.URL, ".jpg"))
		var size int64
		var lastErr error
		for _, candidate := range img.Candidates() {
			size, lastErr = d.downloadGalleryFile(ctx, job.ID, candidate, dir, name)
			if lastErr == nil {
				break
			}
			if ctx.Err() != nil {
				break
			}
			log.Printf("[downloader] job %d: image %s candidate failed (%v), trying next", job.ID, name, lastErr)
		}
		if lastErr != nil {
			d.removeTracker(job.ID)
			if ctx.Err() != nil { // user cancel or shutdown
				d.aborted(ctx, job)
				return
			}
			d.failJob(ctx, job, fmt.Sprintf("download image %s: %v", name, lastErr))
			return
		}
		totalBytes += size
		done++
		tr.downloaded.Store(int64(done))
		media = append(media, galleryAsset{kind: "image", quality: fmt.Sprintf("%04d", i+1),
			path: filepath.Join(dir, name), size: size})
	}

	// Live segments -> live0001.mp4..., kind=video, quality "live"+sequence.
	for i, lv := range detail.LiveVideos {
		if ctx.Err() != nil {
			d.removeTracker(job.ID)
			d.aborted(ctx, job)
			return
		}
		quality := fmt.Sprintf("live%04d", i+1)
		name := quality + extFromURL(lv.URL, ".mp4")
		var size int64
		var lastErr error
		for _, candidate := range lv.Candidates() {
			size, lastErr = d.downloadGalleryFile(ctx, job.ID, candidate, dir, name)
			if lastErr == nil {
				break
			}
			if ctx.Err() != nil {
				break
			}
			log.Printf("[downloader] job %d: live %s candidate failed (%v), trying next", job.ID, name, lastErr)
		}
		if lastErr != nil {
			d.removeTracker(job.ID)
			if ctx.Err() != nil {
				d.aborted(ctx, job)
				return
			}
			d.failJob(ctx, job, fmt.Sprintf("download live %s: %v", name, lastErr))
			return
		}
		totalBytes += size
		done++
		tr.downloaded.Store(int64(done))
		media = append(media, galleryAsset{kind: "video", quality: quality,
			path: filepath.Join(dir, name), size: size})
	}

	// Cover (best effort, fixed cover.jpg name) and metadata.json.
	coverPath, coverSize := d.fetchCoverFixed(ctx, dir, "cover.jpg", detail.CoverURL)
	metaPath, err := writeGalleryMetadata(dir, detail, job)
	if err != nil {
		d.removeTracker(job.ID)
		d.failJob(ctx, job, "write metadata: "+err.Error())
		return
	}

	if err := d.upsertGalleryAssets(ctx, job, media, coverPath, coverSize, metaPath); err != nil {
		d.removeTracker(job.ID)
		d.failJob(ctx, job, "record assets: "+err.Error())
		return
	}

	// Succeeded: the job row carries the accumulated byte totals; the enqueued
	// quality column is left untouched (quality does not apply to galleries).
	d.removeTracker(job.ID)
	finished := nowRFC3339()
	if _, err := d.db.ExecContext(ctx, `
		UPDATE download_jobs SET status = ?, finished_at = ?, total_bytes = ?, downloaded_bytes = ?
		WHERE id = ?`, StatusSucceeded, finished, totalBytes, totalBytes, job.ID); err != nil {
		log.Printf("[downloader] job %d: finalize succeeded: %v", job.ID, err)
	}
	log.Printf("[downloader] job %d: succeeded gallery %s (%d file(s), %d bytes)",
		job.ID, safeName(job.Title, maxTitleLength), totalFiles, totalBytes)
	d.publishStatus(job.ID, job.WorkID, StatusSucceeded, nil)
}

// extFromURL infers a file extension from a media URL (query/fragment
// stripped, lowercase); anything unrecognized falls back to def.
func extFromURL(rawURL, def string) string {
	u := rawURL
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	switch ext := strings.ToLower(filepath.Ext(u)); ext {
	case ".jpg", ".jpeg", ".png", ".webp", ".gif", ".mp4", ".mov", ".m4v", ".webm":
		return ext
	default:
		return def
	}
}

// galleryMetadataDoc is the normalized WorkDetail JSON written as the fixed
// metadata.json inside a gallery directory.
type galleryMetadataDoc struct {
	WorkID     int64                `json:"work_id"`
	ItemID     string               `json:"item_id"`
	Title      string               `json:"title"`
	Type       string               `json:"type"`
	Duration   int                  `json:"duration"`
	CoverURL   string               `json:"cover_url"`
	Downloaded string               `json:"downloaded_at"`
	Images     []provider.WorkImage `json:"images"`
	LiveVideos []provider.LiveVideo `json:"live_videos,omitempty"`
}

// writeGalleryMetadata persists the normalized detail JSON as the fixed
// metadata.json (overwritten on re-download, like the gallery media files).
func writeGalleryMetadata(dir string, detail *provider.WorkDetail, job *jobRow) (string, error) {
	path := filepath.Join(dir, "metadata.json")
	doc := galleryMetadataDoc{
		WorkID:     job.WorkID,
		ItemID:     detail.ItemID,
		Title:      detail.Title,
		Type:       provider.TypeImage,
		Duration:   detail.Duration,
		CoverURL:   detail.CoverURL,
		Downloaded: nowRFC3339(),
		Images:     detail.Images,
		LiveVideos: detail.LiveVideos,
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// aborted finishes a canceled transfer. User cancel -> status=canceled +
// .part cleanup (already done by the caller). Shutdown -> the row stays
// downloading on purpose: RecoverStale requeues it on the next start.
func (d *Downloader) aborted(ctx context.Context, job *jobRow) {
	if d.baseCtx.Err() != nil {
		log.Printf("[downloader] job %d: interrupted by shutdown, row left downloading for restart recovery", job.ID)
		return
	}
	finished := nowRFC3339()
	if _, err := d.db.ExecContext(context.WithoutCancel(ctx), `
		UPDATE download_jobs SET status = ?, finished_at = ?, error = 'canceled by user'
		WHERE id = ?`, StatusCanceled, finished, job.ID); err != nil {
		log.Printf("[downloader] job %d: finalize canceled: %v", job.ID, err)
	}
	log.Printf("[downloader] job %d: canceled", job.ID)
	d.publishStatus(job.ID, job.WorkID, StatusCanceled, strPtr("canceled by user"))
}

// failJob records a terminal failure.
func (d *Downloader) failJob(ctx context.Context, job *jobRow, msg string) {
	fctx := context.WithoutCancel(ctx)
	finished := nowRFC3339()
	if _, err := d.db.ExecContext(fctx, `
		UPDATE download_jobs SET status = ?, finished_at = ?, error = ?
		WHERE id = ?`, StatusFailed, finished, msg, job.ID); err != nil {
		log.Printf("[downloader] job %d: finalize failed: %v", job.ID, err)
	}
	log.Printf("[downloader] job %d: failed: %s", job.ID, msg)
	d.publishStatus(job.ID, job.WorkID, StatusFailed, strPtr(msg))
}

// --------------------------------------------------------------- transfers --

// nameMu serializes final renames within this process: the rename IS the
// filename reservation, so two concurrent jobs with the same title cannot
// collapse onto one target (the loser re-picks "title-(2).mp4" inside the
// critical section because the winner's file already exists).
var nameMu sync.Mutex

// browserUA mimics a desktop Chrome request; the douyin CDN rejects bare
// clients (403) regardless of signature validity.
const browserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// mediaHeaders builds the headers the douyin CDN expects on media requests:
// Referer + UA are mandatory, the session cookie unlocks authenticated nodes.
// The cookie is re-read from the cookie file on every call so a settings
// update takes effect without restart (same contract as the sidecar).
func (d *Downloader) mediaHeaders() http.Header {
	h := http.Header{}
	h.Set("Referer", "https://www.douyin.com/")
	h.Set("User-Agent", browserUA)
	if d.store != nil {
		if data, err := os.ReadFile(d.store.CookieFilePath()); err == nil {
			if c := strings.TrimSpace(string(data)); c != "" {
				h.Set("Cookie", c)
			}
		}
	}
	return h
}

// downloadVideo streams url into dir/name (via a unique .<jobID>.part file)
// and renames it into place, returning the final path. On error the .part
// file is removed (cancel/abort leaves nothing behind).
func (d *Downloader) downloadVideo(ctx context.Context, jobID int64, rawURL, dir, name string, tr *tracker) (int64, string, error) {
	return d.streamToFile(ctx, jobID, rawURL, dir, name, tr, false)
}

// downloadGalleryFile streams url into dir/name verbatim: image galleries use
// deterministic names (0001.jpg, live0001.mp4) that overwrite on re-download.
// tr stays nil: gallery progress is counted per finished file by the caller,
// not per byte inside one transfer.
func (d *Downloader) downloadGalleryFile(ctx context.Context, jobID int64, rawURL, dir, name string) (int64, error) {
	written, _, err := d.streamToFile(ctx, jobID, rawURL, dir, name, nil, true)
	return written, err
}

// streamToFile GETs rawURL (media headers) and streams the body into dir/name
// through a .<jobID>.part file, then renames it into place. fixed=false
// re-picks name-(2).ext under the reservation lock (video layout: concurrent
// same-title jobs must not collapse); fixed=true writes the name verbatim and
// overwrites (gallery layout: deterministic 0001.jpg targets). tr may be nil
// (gallery transfers are counted per file by the caller). Returns the written
// byte count and the final path.
func (d *Downloader) streamToFile(ctx context.Context, jobID int64, rawURL, dir, name string, tr *tracker, fixed bool) (int64, string, error) {
	url := d.resolveMedia(rawURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, "", err
	}
	for k, vs := range d.mediaHeaders() {
		req.Header[k] = vs
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if tr != nil {
		if n := resp.ContentLength; n > 0 {
			tr.total.Store(n)
		}
	}

	part := filepath.Join(dir, fmt.Sprintf("%s.%d.part", name, jobID))
	defer func() {
		if part != "" {
			_ = os.Remove(part)
		}
	}()

	f, err := os.Create(part)
	if err != nil {
		return 0, "", err
	}

	buf := make([]byte, 32*1024)
	var written int64
	var loopErr error
	for loopErr == nil {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				loopErr = werr
				break
			}
			written += int64(n)
			if tr != nil {
				tr.downloaded.Store(written)
			}
		}
		switch {
		case rerr != nil && errors.Is(rerr, io.EOF):
			// complete body
		case rerr != nil:
			loopErr = rerr // includes ctx cancellation from the request
		case ctx.Err() != nil:
			loopErr = ctx.Err()
		default:
			continue
		}
		break
	}
	// Single close point (a double Close would shadow the real outcome).
	if cerr := f.Close(); loopErr == nil {
		loopErr = cerr
	}
	if loopErr != nil {
		return written, "", loopErr
	}
	if written == 0 {
		return 0, "", errors.New("empty response body")
	}

	nameMu.Lock()
	var final string
	if fixed {
		final = filepath.Join(dir, name) // deterministic target, may overwrite
		err = os.Rename(part, final)
	} else {
		final, err = uniquePath(dir, name) // re-pick under the lock: winner exists now
		if err == nil {
			err = os.Rename(part, final)
		}
	}
	nameMu.Unlock()
	if err != nil {
		return written, "", err
	}
	part = "" // renamed away; the deferred remove must not touch it
	return written, final, nil
}

// fetchCover downloads the cover image (best effort). Returns the path and
// size, or ("", 0) when skipped/failed. The extension is picked from the URL
// (unknown -> .jpg) and conflicts get a -(2) sequence number (video layout).
func (d *Downloader) fetchCover(ctx context.Context, dir, stem, coverURL string) (string, int64) {
	if strings.TrimSpace(coverURL) == "" {
		return "", 0
	}
	ext := strings.ToLower(filepath.Ext(coverURL))
	switch ext {
	case ".jpg", ".jpeg", ".png", ".webp", ".gif":
	default:
		ext = ".jpg"
	}
	target, err := uniquePath(dir, stem+ext)
	if err != nil {
		log.Printf("[downloader] cover: resolve target: %v", err)
		return "", 0
	}
	return d.fetchToFile(ctx, target, coverURL)
}

// fetchCoverFixed writes dir/name verbatim: image galleries keep the
// deterministic cover.jpg name and overwrite it on re-download.
func (d *Downloader) fetchCoverFixed(ctx context.Context, dir, name, coverURL string) (string, int64) {
	if strings.TrimSpace(coverURL) == "" {
		return "", 0
	}
	return d.fetchToFile(ctx, filepath.Join(dir, name), coverURL)
}

// fetchToFile performs the best-effort cover/gallery fetch: GET with media
// headers, stream into target, remove the file on any failure. Returns the
// target path and written size, or ("", 0).
func (d *Downloader) fetchToFile(ctx context.Context, target, rawURL string) (string, int64) {
	url := d.resolveMedia(rawURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		log.Printf("[downloader] cover: build request: %v", err)
		return "", 0
	}
	for k, vs := range d.mediaHeaders() {
		req.Header[k] = vs
	}
	resp, err := d.client.Do(req)
	if err != nil {
		log.Printf("[downloader] cover: fetch: %v", err)
		return "", 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("[downloader] cover: HTTP %d", resp.StatusCode)
		return "", 0
	}

	f, err := os.Create(target)
	if err != nil {
		log.Printf("[downloader] cover: create: %v", err)
		return "", 0
	}
	n, err := io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil || n == 0 {
		log.Printf("[downloader] cover: write: %v", err)
		_ = os.Remove(target)
		return "", 0
	}
	return target, n
}

// metadataDoc is the normalized WorkDetail JSON written next to the video.
type metadataDoc struct {
	WorkID     int64              `json:"work_id"`
	ItemID     string             `json:"item_id"`
	Title      string             `json:"title"`
	Duration   int                `json:"duration"`
	Quality    string             `json:"selected_quality"`
	SizeBytes  int64              `json:"selected_size_bytes"`
	CoverURL   string             `json:"cover_url"`
	Downloaded string             `json:"downloaded_at"`
	Variants   []provider.Variant `json:"variants"`
}

// writeMetadata persists the normalized detail JSON as <stem>.metadata.json.
func writeMetadata(dir, videoPath string, detail *provider.WorkDetail, job *jobRow, v *provider.Variant) (string, error) {
	stem := strings.TrimSuffix(filepath.Base(videoPath), ".mp4")
	path, err := uniquePath(dir, stem+".metadata.json")
	if err != nil {
		return "", err
	}
	doc := metadataDoc{
		WorkID:     job.WorkID,
		ItemID:     detail.ItemID,
		Title:      detail.Title,
		Duration:   detail.Duration,
		Quality:    v.Quality,
		SizeBytes:  v.SizeBytes,
		CoverURL:   detail.CoverURL,
		Downloaded: nowRFC3339(),
		Variants:   detail.Variants,
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// assetFiles bundles the produced files for the asset rows.
type assetFiles struct {
	video     string
	videoSize int64
	quality   string
	cover     string
	coverSize int64
	metadata  string
}

// upsertAssets records video/cover/metadata rows in one transaction. Video
// rows upsert on UNIQUE(work_id, kind, quality); cover/metadata carry NULL
// quality (NULL never conflicts in SQLite) and are delete+insert.
func (d *Downloader) upsertAssets(ctx context.Context, job *jobRow, f assetFiles) error {
	fctx := context.WithoutCancel(ctx)
	now := nowRFC3339()
	metaSize := int64(0)
	if fi, err := os.Stat(f.metadata); err == nil {
		metaSize = fi.Size()
	}
	return db.WithTx(fctx, d.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(fctx, `
			INSERT INTO assets (work_id, kind, path, size_bytes, quality, created_at)
			VALUES (?, 'video', ?, ?, ?, ?)
			ON CONFLICT(work_id, kind, quality) DO UPDATE SET
				path = excluded.path, size_bytes = excluded.size_bytes, created_at = excluded.created_at`,
			job.WorkID, d.relPath(f.video), f.videoSize, f.quality, now); err != nil {
			return fmt.Errorf("video asset: %w", err)
		}
		for _, a := range []struct {
			kind string
			path string
			size int64
		}{
			{"cover", f.cover, f.coverSize},
			{"metadata", f.metadata, metaSize},
		} {
			if a.path == "" {
				continue
			}
			if _, err := tx.ExecContext(fctx,
				`DELETE FROM assets WHERE work_id = ? AND kind = ? AND quality IS NULL`, job.WorkID, a.kind); err != nil {
				return fmt.Errorf("clear %s asset: %w", a.kind, err)
			}
			if _, err := tx.ExecContext(fctx, `
				INSERT INTO assets (work_id, kind, path, size_bytes, quality, created_at)
				VALUES (?, ?, ?, ?, NULL, ?)`, job.WorkID, a.kind, d.relPath(a.path), a.size, now); err != nil {
				return fmt.Errorf("%s asset: %w", a.kind, err)
			}
		}
		return nil
	})
}

// galleryAsset is one produced gallery media file awaiting its asset row.
type galleryAsset struct {
	kind    string // "image" or "video" (live segments)
	quality string // "0001"-style sequence or "live0001"
	path    string
	size    int64
}

// upsertGalleryAssets records the asset rows of an image work in one
// transaction. The gallery media set is replaced wholesale (stale rows from a
// previous, larger download must not linger), cover/metadata keep the
// delete+insert convention of the video layout.
func (d *Downloader) upsertGalleryAssets(ctx context.Context, job *jobRow, media []galleryAsset, cover string, coverSize int64, metadata string) error {
	fctx := context.WithoutCancel(ctx)
	now := nowRFC3339()
	metaSize := int64(0)
	if fi, err := os.Stat(metadata); err == nil {
		metaSize = fi.Size()
	}
	return db.WithTx(fctx, d.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(fctx,
			`DELETE FROM assets WHERE work_id = ? AND kind IN ('image', 'video')`, job.WorkID); err != nil {
			return fmt.Errorf("clear gallery assets: %w", err)
		}
		for _, m := range media {
			if _, err := tx.ExecContext(fctx, `
				INSERT INTO assets (work_id, kind, path, size_bytes, quality, created_at)
				VALUES (?, ?, ?, ?, ?, ?)`,
				job.WorkID, m.kind, d.relPath(m.path), m.size, m.quality, now); err != nil {
				return fmt.Errorf("%s asset %s: %w", m.kind, m.quality, err)
			}
		}
		for _, a := range []struct {
			kind string
			path string
			size int64
		}{
			{"cover", cover, coverSize},
			{"metadata", metadata, metaSize},
		} {
			if a.path == "" {
				continue
			}
			if _, err := tx.ExecContext(fctx,
				`DELETE FROM assets WHERE work_id = ? AND kind = ? AND quality IS NULL`, job.WorkID, a.kind); err != nil {
				return fmt.Errorf("clear %s asset: %w", a.kind, err)
			}
			if _, err := tx.ExecContext(fctx, `
				INSERT INTO assets (work_id, kind, path, size_bytes, quality, created_at)
				VALUES (?, ?, ?, ?, NULL, ?)`, job.WorkID, a.kind, d.relPath(a.path), a.size, now); err != nil {
				return fmt.Errorf("%s asset: %w", a.kind, err)
			}
		}
		return nil
	})
}

// ------------------------------------------------------------------ paths --

// directory renders <dataDir>/downloads/{nickname}_{sec_uid}/{collections|singles}.
func (j *jobRow) directory(dataDir string) string {
	return j.collectionBase(dataDir)
}

// collectionBase renders the per-creator bucket directory
// <dataDir>/downloads/{nickname}_{sec_uid}/{collections|singles}; image works
// append one more segment (safeName(title)) to aggregate the gallery.
func (j *jobRow) collectionBase(dataDir string) string {
	sub := "singles"
	if j.CollectionID.Valid {
		sub = "collections"
	}
	nick := safeName(j.Nickname, maxNameLength)
	if nick == "untitled" {
		nick = "unknown"
	}
	sec := j.SecUID
	if sec == "" {
		sec = "nosec"
	}
	return filepath.Join(dataDir, "downloads", nick+"_"+sec, sub)
}

// relPath converts an absolute path under the data dir to a portable
// forward-slash relative path (stored in assets.path).
func (d *Downloader) relPath(full string) string {
	prefix := d.dataDir
	if prefix != "" {
		if rel, err := filepath.Rel(prefix, full); err == nil &&
			rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(full)
}

// maxNameLength/maxTitleLength cap path segments against Windows MAX_PATH.
const (
	maxNameLength  = 60
	maxTitleLength = 80
)

// untitledFallback returns itemID when the work has no usable title, so
// untitled gallery folders stay unique instead of piling into "untitled-(2)".
func untitledFallback(title, itemID string) string {
	if strings.TrimSpace(title) == "" {
		return itemID
	}
	return title
}

// safeName renders a Windows-safe file/dir name: reserved characters and
// control characters become "_", trailing dots/spaces are trimmed, reserved
// device names are prefixed, length is capped.
func safeName(s string, max int) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < 0x20 || r == 0x7f:
			b.WriteByte('_')
		case strings.ContainsRune(`<>:"/\|?*`, r):
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	s = b.String()
	// 按字符数截断(不是字节):中文/emoji 标题的字节数远大于字符数,
	// 用 len(s) 判断会对多字节标题触发 slice bounds panic(真实数据已踩过)
	if runes := []rune(s); len(runes) > max {
		s = string(runes[:max])
	}
	s = strings.TrimRight(s, " .")
	switch strings.ToUpper(s) {
	case "CON", "PRN", "AUX", "NUL",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		s = "_" + s
	}
	if s == "" {
		s = "untitled"
	}
	return s
}

// uniquePath returns dir/name, or dir/stem-(2)ext, -(3)... for the first
// non-existing candidate (contract: "conflicts get a -(2) sequence number").
func uniquePath(dir, name string) (string, error) {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	candidate := filepath.Join(dir, name)
	for i := 2; ; i++ {
		_, err := os.Stat(candidate)
		if errors.Is(err, fs.ErrNotExist) {
			return candidate, nil
		}
		if err != nil {
			return "", err
		}
		candidate = filepath.Join(dir, fmt.Sprintf("%s-(%d)%s", stem, i, ext))
	}
}
