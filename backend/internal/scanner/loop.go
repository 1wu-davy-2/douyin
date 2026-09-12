package scanner

// Scan execution: the cursor loop, page persistence and finalization.
//
// Termination conditions of the cursor loop (loopState.terminate):
//
//	termNatural     - provider said has_more=false (clean end; reconciliation
//	                  decides succeeded vs partial by completeness gap)
//	termIncremental - !full and N consecutive fully-known pages (early stop;
//	                  reconciliation applies as well)
//	termBadPages    - settings.scan_max_empty_pages consecutive bad pages
//	                  (provider error / missing fields even after retries)
//	termStuck       - has_more=true but the next cursor is empty/repeated AND
//	                  the fallback cursor is exhausted AND the current round
//	                  produced no new works and no new cursor
//	termCap         - paranoid per-run request cap (upstream infinite loop)
//	termDB          - a database error while persisting a page
//	termCanceled    - context canceled (process shutdown)
//
// Final status mapping: termCanceled/termDB -> failed; termBadPages/termStuck/
// termCap -> partial; termNatural/termIncremental -> succeeded unless the
// completeness gap exceeds the threshold (then partial). pages==0 downgrades a
// partial to failed (nothing was ever scanned).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"strconv"
	"strings"
	"time"

	"douyin/backend/internal/events"
	"douyin/backend/internal/provider"
)

const (
	pageCount = 20 // sidecar contract page size

	// retryBackoffs are the sleeps between bad-page retries on the same
	// cursor: initial attempt + up to 3 retries (2s / 4s / 8s).
	retryBackoffCount = 3

	// maxRequestsPerScan is a paranoia valve against providers that keep
	// claiming has_more with fresh works forever; unreachable in practice.
	maxRequestsPerScan = 100000
)

// retryBackoff returns the backoff before retry i (0-based): 2s, 4s, 8s.
func retryBackoff(i int) time.Duration {
	return time.Duration(1<<uint(i+1)) * time.Second
}

// Termination reasons.
const (
	termNatural     = "natural"
	termIncremental = "incremental_stop"
	termBadPages    = "bad_pages"
	termStuck       = "cursor_stuck"
	termCap         = "request_cap"
	termDB          = "db_error"
	termCanceled    = "canceled"
)

// loopState is the mutable cursor-loop state.
type loopState struct {
	cursor string

	// requested holds every cursor already requested, including synthesized
	// fallback cursors.
	requested map[string]bool

	// segOldestMs is the oldest published_at (unix ms) seen in the current
	// segment, i.e. since the last fallback; 0 = unknown.
	segOldestMs int64

	// roundNew / roundCursor count progress (new works / new server cursors)
	// since the last fallback. A round without any progress means the
	// pagination has truly bottomed out.
	roundNew    int
	roundCursor int

	reqCount int

	steps        int // page steps attempted (success or failed), = scan_runs.pages
	newCount     int
	updatedCount int
	emptyPages   int
	consecBad    int
	consecKnown  int
	lastErr      string
	terminate    string
}

// advance computes the next cursor after a page whose next_cursor was nc
// (possibly empty). It returns (next, true) or ("", false) when no progress is
// possible anymore. Rules (in order):
//
//  1. A fresh (never requested) next cursor -> use it.
//  2. Otherwise fall back to segOldestMs-1 if that cursor was never requested.
//     Unlimited fallbacks; resets the segment and the round counters.
//  3. Otherwise, if the current round still made progress (new works or a new
//     cursor), re-request the most promising known cursor once as a last
//     resort. A repeated cursor with fresh works must not terminate the scan.
//  4. Otherwise the pagination is exhausted: no fresh cursor, no fallback, no
//     progress in a whole round -> give up.
func (l *loopState) advance(nc string) (string, bool) {
	nc = strings.TrimSpace(nc)
	if nc != "" && !l.requested[nc] {
		l.requested[nc] = true
		l.roundCursor++
		l.cursor = nc
		return nc, true
	}

	if l.segOldestMs > 0 {
		fb := strconv.FormatInt(l.segOldestMs-1, 10)
		if !l.requested[fb] {
			l.requested[fb] = true
			l.cursor = fb
			l.segOldestMs = 0
			l.roundNew, l.roundCursor = 0, 0
			return fb, true
		}
	}

	if l.roundNew > 0 || l.roundCursor > 0 {
		target := nc
		if target == "" && l.segOldestMs > 0 {
			target = strconv.FormatInt(l.segOldestMs-1, 10)
		}
		if target == "" {
			target = l.cursor
		}
		if target != "" {
			l.requested[target] = true
			l.cursor = target
			l.roundNew, l.roundCursor = 0, 0
			return target, true
		}
	}
	return "", false
}

// classifyPage returns "" when the page is usable, or the reason it is bad.
// A page is bad when the items field is missing, every item lacks item_id, or
// it is empty while claiming has_more (the douyin mid-pagination hole).
func classifyPage(p *provider.PostsPage) string {
	if p == nil {
		return "nil page"
	}
	if p.Items == nil {
		return "items field missing"
	}
	valid := 0
	for _, it := range p.Items {
		if strings.TrimSpace(it.ItemID) != "" {
			valid++
		}
	}
	if valid == 0 && p.HasMore {
		return "no usable items but has_more=true"
	}
	return ""
}

// fetchPage requests one page with bad-page retries: up to 1+3 attempts on the
// same cursor with exponential backoff. Returns the page, the next cursor
// observed from any (bad) attempt, and an error when all attempts failed.
func (s *Scanner) fetchPage(ctx context.Context, prov provider.Provider, secUID, cursor string) (*provider.PostsPage, string, error) {
	var observedNext string
	var lastErr error
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			if serr := s.sleep(ctx, retryBackoff(attempt-1)); serr != nil {
				return nil, observedNext, fmt.Errorf("aborted during retry backoff: %w", serr)
			}
		}
		page, err := prov.PostsPage(ctx, secUID, cursor, pageCount)
		if err == nil {
			if reason := classifyPage(page); reason == "" {
				return page, "", nil
			} else {
				lastErr = errors.New(reason)
				if page.NextCursor != nil {
					observedNext = *page.NextCursor
				}
			}
		} else {
			lastErr = err
		}
		if attempt >= retryBackoffCount {
			break
		}
	}
	return nil, observedNext, lastErr
}

// execute runs the whole scan for one creator and returns the final result.
// It never leaves a scan_runs row in status=running. Cancellation comes from
// the scanner's baseCtx (process shutdown); fctx detaches the finalization
// writes from it.
func (s *Scanner) execute(scanID, creatorID int64, secUID string, full bool, trigger string) Result {
	ctx := s.baseCtx // aborts the loop on process shutdown
	res := Result{
		ScanID:    scanID,
		CreatorID: creatorID,
		Status:    StatusFailed,
		Trigger:   trigger,
		Full:      full,
	}

	// Global scan concurrency (settings.scan_concurrency, 1-5): queue behind
	// running scans; each creator additionally stays single-flight via active.
	if err := s.acquireSlot(s.baseCtx); err != nil {
		res.LastError = strPtr("scan canceled before start")
		s.writeFinalRow(res)
		s.publishDone(res)
		return res
	}
	defer s.releaseSlot()

	// fctx detaches from scan cancellation so finalization (scan_runs row,
	// scan.done event, enqueue) still lands when the process is shutting down.
	fctx := context.WithoutCancel(s.baseCtx)

	st := &loopState{cursor: "0", requested: map[string]bool{"0": true}}

	// --- settings (defaults mirror the documented ranges) --------------------
	gapThreshold, incStop, maxEmpty := 5, 3, 3
	var delayBase time.Duration = 2000 * time.Millisecond
	if view, err := s.store.View(fctx); err != nil {
		log.Printf("[scanner] scan %d: read settings (%v), using defaults", scanID, err)
	} else {
		gapThreshold = view.CompletenessGapThreshold
		incStop = view.IncrementalStopPages
		maxEmpty = view.ScanMaxEmptyPages
		delayBase = time.Duration(view.ScanPageDelayMs) * time.Millisecond
	}

	prov := s.src.Resolve(fctx)
	// The page pacing exists to be gentle to the real upstream only.
	if _, isMock := prov.(*provider.MockProvider); isMock {
		delayBase = 0
	}

	// --- step 1: refresh the profile (failure must not block the scan) -------
	reported := 0
	if prof, err := prov.Profile(fctx, secUID); err != nil {
		// "记 0": the completeness gap for this run is computed against 0;
		// the stored reported_work_count is kept for display purposes.
		log.Printf("[scanner] scan %d: profile refresh failed (continuing, reported=0): %v", scanID, err)
	} else {
		reported = prof.AwemeCount
		if _, err := s.db.ExecContext(fctx,
			`UPDATE creators SET nickname = ?, avatar_url = ?, reported_work_count = ? WHERE id = ?`,
			prof.Nickname, prof.AvatarURL, reported, creatorID); err != nil {
			log.Printf("[scanner] scan %d: update creator profile: %v", scanID, err)
		}
	}

	// --- preload known works (id per item, including soft-deleted ones) ------
	known := make(map[string]int64)
	seen := make(map[int64]bool)
	func() {
		rows, err := s.db.QueryContext(fctx,
			`SELECT id, item_id FROM works WHERE creator_id = ?`, creatorID)
		if err != nil {
			log.Printf("[scanner] scan %d: preload works: %v", scanID, err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			var itemID string
			if err := rows.Scan(&id, &itemID); err == nil {
				known[itemID] = id
			}
		}
	}()

	// --- one connection reused for the whole scan -----------------------------
	conn, err := s.db.Conn(fctx)
	if err != nil {
		res.LastError = strPtr("acquire connection: " + err.Error())
		res = s.finalize(fctx, res, creatorID, reported, gapThreshold, nil, seen, st)
		return res
	}
	defer conn.Close()

	var newWorkIDs []int64

	for st.terminate == "" {
		if err := ctx.Err(); err != nil {
			st.terminate = termCanceled
			st.lastErr = "scan canceled: " + err.Error()
			break
		}
		st.reqCount++
		if st.reqCount > maxRequestsPerScan {
			st.terminate = termCap
			st.lastErr = fmt.Sprintf("request cap (%d) exceeded, aborting", maxRequestsPerScan)
			break
		}

		page, observedNext, ferr := s.fetchPage(ctx, prov, secUID, st.cursor)
		if ferr != nil {
			// Bad page: retries exhausted. Count it, remember why, and either
			// advance (fallback/observed cursor) or stay and let the next
			// step retry the same cursor.
			st.steps++
			st.emptyPages++
			st.consecBad++
			st.lastErr = fmt.Sprintf("page %d (cursor %s) failed after %d attempt(s): %v",
				st.steps, st.cursor, retryBackoffCount+1, ferr)
			if st.consecBad >= maxEmpty {
				st.terminate = termBadPages
				break
			}
			if next, ok := st.advance(observedNext); ok {
				st.cursor = next
			}
			s.emitProgress(ctx, conn, st, scanID, creatorID)
			s.sleepDelay(ctx, delayBase)
			continue
		}

		st.consecBad = 0
		st.steps++
		newN, updN, pageNewIDs, oldestMs, perr := persistPage(ctx, conn, creatorID, page, known, seen)
		if perr != nil {
			st.terminate = termDB
			st.lastErr = fmt.Sprintf("persist page %d (cursor %s): %v", st.steps, st.cursor, perr)
			break
		}
		st.newCount += newN
		st.updatedCount += updN
		st.roundNew += newN
		newWorkIDs = append(newWorkIDs, pageNewIDs...)
		if oldestMs > 0 && (st.segOldestMs == 0 || oldestMs < st.segOldestMs) {
			st.segOldestMs = oldestMs
		}

		// Incremental early stop: !full and a full page of already-known works.
		if newN > 0 {
			st.consecKnown = 0
		}
		if !full && len(page.Items) > 0 && newN == 0 {
			st.consecKnown++
			if st.consecKnown >= incStop {
				st.terminate = termIncremental
			}
		}

		s.emitProgress(ctx, conn, st, scanID, creatorID)
		if st.terminate != "" { // incremental stop
			break
		}

		if !page.HasMore {
			st.terminate = termNatural
			break
		}

		nextCursor := ""
		if page.NextCursor != nil {
			nextCursor = *page.NextCursor
		}
		next, ok := st.advance(nextCursor)
		if !ok {
			st.terminate = termStuck
			st.lastErr = fmt.Sprintf(
				"cursor stuck at %s: has_more=true but next cursor empty/repeated and fallback exhausted without progress",
				st.cursor)
			break
		}
		st.cursor = next
		s.sleepDelay(ctx, delayBase)
	}

	res.Pages = st.steps
	res.NewCount = st.newCount
	res.UpdatedCount = st.updatedCount
	res.EmptyPages = st.emptyPages
	res = s.finalize(fctx, res, creatorID, reported, gapThreshold, newWorkIDs, seen, st)
	return res
}

// sleepDelay pauses page-delay ms +- 30% between pages (jitter to look less
// robotic); no-op when the base delay is 0 (mock provider).
func (s *Scanner) sleepDelay(ctx context.Context, base time.Duration) {
	if base <= 0 {
		return
	}
	d := time.Duration(float64(base) * (0.7 + 0.6*rand.Float64()))
	_ = s.sleep(ctx, d)
}

// emitProgress persists the per-page progress columns on the scan's own
// connection and publishes scan.progress (progress-class: dropped for slow
// SSE consumers, never blocks the scan).
func (s *Scanner) emitProgress(ctx context.Context, conn *sql.Conn, st *loopState, scanID, creatorID int64) {
	if _, err := conn.ExecContext(ctx,
		`UPDATE scan_runs SET pages = ?, new_count = ?, updated_count = ?, empty_pages = ? WHERE id = ?`,
		st.steps, st.newCount, st.updatedCount, st.emptyPages, scanID); err != nil {
		log.Printf("[scanner] scan %d: update progress: %v", scanID, err)
	}
	if s.bus == nil {
		return
	}
	s.bus.Publish(events.Event{
		Type: events.TypeScanProgress,
		Data: events.ScanProgress{
			ScanID:       scanID,
			CreatorID:    creatorID,
			Page:         st.steps,
			NewCount:     st.newCount,
			UpdatedCount: st.updatedCount,
			Status:       StatusRunning,
		},
	})
}

// persistPage upserts one page of works (and their collections) in a single
// transaction. Returns per-page counts, the ids of newly inserted works and
// the oldest published_at (unix ms) of the page. duration arrives in
// milliseconds from the sidecar and is stored in seconds.
func persistPage(ctx context.Context, conn *sql.Conn, creatorID int64, page *provider.PostsPage,
	known map[string]int64, seen map[int64]bool) (newN, updN int, newIDs []int64, oldestMs int64, err error) {

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, nil, 0, fmt.Errorf("begin page tx: %w", err)
	}
	defer func() {
		if rerr := tx.Rollback(); rerr != nil && !errors.Is(rerr, sql.ErrTxDone) && err == nil {
			err = fmt.Errorf("rollback page tx: %w", rerr)
		}
	}()

	now := nowRFC3339()
	pageSeen := make(map[string]bool, len(page.Items))
	for _, it := range page.Items {
		itemID := strings.TrimSpace(it.ItemID)
		if itemID == "" || pageSeen[itemID] {
			continue // drop items without id / duplicated within the page
		}
		pageSeen[itemID] = true

		var collectionID any
		if mixID := strings.TrimSpace(it.MixID); mixID != "" {
			cid, cerr := upsertCollectionTx(ctx, tx, creatorID, mixID, it.MixName, it.CoverURL, now)
			if cerr != nil {
				return 0, 0, nil, 0, cerr
			}
			collectionID = cid
		}

		var published any
		if it.PublishedAt != "" {
			published = it.PublishedAt
		}
		if ms, ok := publishedAtMillis(it.PublishedAt); ok && (oldestMs == 0 || ms < oldestMs) {
			oldestMs = ms
		}

		durationSec := it.Duration / 1000 // sidecar carries milliseconds

		// Work type (stage 9): the sidecar classifies galleries
		// ("aweme.images" non-empty -> image). Unknown/empty -> video.
		workType := it.Type
		if workType != provider.TypeImage && workType != provider.TypeVideo {
			workType = provider.TypeVideo
		}

		id, existed := known[itemID]
		if existed {
			updN++
			if _, uerr := tx.ExecContext(ctx, `
				UPDATE works SET
					collection_id = COALESCE(?, collection_id),
					title = ?, cover_url = ?, duration = ?, type = ?, published_at = ?,
					image_count = ?, deleted_at = NULL, updated_at = ?
				WHERE id = ?`,
				collectionID, it.Title, it.CoverURL, durationSec, workType, published, it.ImageCount, now, id); uerr != nil {
				return 0, 0, nil, 0, fmt.Errorf("update work %s: %w", itemID, uerr)
			}
		} else {
			newN++
			// UPSERT keyed by UNIQUE(creator_id, item_id); a conflict can only
			// happen with a concurrent writer, which the single-flight rule
			// excludes - the DO UPDATE branch is a safety net.
			if _, ierr := tx.ExecContext(ctx, `
				INSERT INTO works (creator_id, collection_id, item_id, title, cover_url,
				                   duration, type, image_count, published_at, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(creator_id, item_id) DO UPDATE SET
					collection_id = COALESCE(excluded.collection_id, works.collection_id),
					title = excluded.title,
					cover_url = excluded.cover_url,
					duration = excluded.duration,
					type = excluded.type,
					image_count = excluded.image_count,
					published_at = excluded.published_at,
					deleted_at = NULL,
					updated_at = excluded.updated_at`,
				creatorID, collectionID, itemID, it.Title, it.CoverURL,
				durationSec, workType, it.ImageCount, published, now, now); ierr != nil {
				return 0, 0, nil, 0, fmt.Errorf("insert work %s: %w", itemID, ierr)
			}
			if serr := tx.QueryRowContext(ctx,
				`SELECT id FROM works WHERE creator_id = ? AND item_id = ?`, creatorID, itemID).Scan(&id); serr != nil {
				return 0, 0, nil, 0, fmt.Errorf("read back work %s: %w", itemID, serr)
			}
			known[itemID] = id
			newIDs = append(newIDs, id)
		}
		seen[id] = true
	}

	if err = tx.Commit(); err != nil {
		return 0, 0, nil, 0, fmt.Errorf("commit page tx: %w", err)
	}
	return newN, updN, newIDs, oldestMs, nil
}

// upsertCollectionTx inserts or updates a collection row inside the page
// transaction and returns its id.
func upsertCollectionTx(ctx context.Context, tx *sql.Tx, creatorID int64, mixID, name, coverURL, now string) (int64, error) {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO collections (creator_id, mix_id, name, cover_url, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(creator_id, mix_id) DO UPDATE SET
			name = CASE WHEN excluded.name != '' THEN excluded.name ELSE collections.name END,
			cover_url = CASE WHEN excluded.cover_url != '' THEN excluded.cover_url ELSE collections.cover_url END`,
		creatorID, mixID, name, coverURL, now); err != nil {
		return 0, fmt.Errorf("upsert collection %s: %w", mixID, err)
	}
	var id int64
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM collections WHERE creator_id = ? AND mix_id = ?`, creatorID, mixID).Scan(&id); err != nil {
		return 0, fmt.Errorf("read back collection %s: %w", mixID, err)
	}
	return id, nil
}

// publishedAtMillis parses an RFC3339 timestamp into unix milliseconds.
func publishedAtMillis(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0, false
	}
	return t.UnixMilli(), true
}

// finalize resolves the final status, runs the completeness reconciliation,
// soft-deletes vanished works after a successful full scan, writes the final
// scan_runs row and publishes scan.done. It then hands new works to the
// subscription auto-download matcher. All DB writes use fctx so shutdown
// cannot strand the run in status=running. Returns the updated Result.
func (s *Scanner) finalize(fctx context.Context, res Result, creatorID int64,
	reported, gapThreshold int, newWorkIDs []int64, seen map[int64]bool,
	st *loopState) Result {

	res.Status = StatusSucceeded
	switch st.terminate {
	case termCanceled, termDB, "":
		// "" cannot happen (every loop exit sets a reason); treat defensively
		// as failed so a running row can never leak.
		res.Status = StatusFailed
	case termBadPages, termStuck, termCap:
		// Spec: even a scan where every page failed ends as partial, not
		// failed - the interruption itself is the diagnosable condition.
		res.Status = StatusPartial
	case termNatural, termIncremental:
		// reconciliation decides below
	}

	// Completeness reconciliation: gap = reported - stored non-deleted works.
	if res.Status != StatusFailed {
		var inDB int
		if err := s.db.QueryRowContext(fctx,
			`SELECT COUNT(*) FROM works WHERE creator_id = ? AND deleted_at IS NULL`, creatorID).Scan(&inDB); err != nil {
			log.Printf("[scanner] scan %d: count works: %v", res.ScanID, err)
		} else {
			gap := reported - inDB
			if gap < 0 {
				gap = 0
			}
			res.Completeness = gap
			if gap > gapThreshold && res.Status == StatusSucceeded {
				res.Status = StatusPartial
				if st.lastErr == "" {
					st.lastErr = fmt.Sprintf("completeness gap %d: profile reports %d works, %d in library",
						gap, reported, inDB)
				}
			}
		}
	}

	// Soft delete: only a SUCCESSFUL full scan may hide works it did not see.
	if res.Full && res.Status == StatusSucceeded {
		q := `UPDATE works SET deleted_at = ? WHERE creator_id = ? AND deleted_at IS NULL`
		args := []any{nowRFC3339(), creatorID}
		if ids := seenIDs(seen); len(ids) > 0 {
			q += ` AND id NOT IN (` + placeholders(len(ids)) + `)`
			for _, id := range ids {
				args = append(args, id)
			}
		}
		if _, err := s.db.ExecContext(fctx, q, args...); err != nil {
			log.Printf("[scanner] scan %d: soft delete: %v", res.ScanID, err)
		}
	}

	res.LastError = nil
	if st.lastErr != "" {
		res.LastError = strPtr(st.lastErr)
	}

	if err := s.writeFinalRow(res); err != nil {
		log.Printf("[scanner] scan %d: finalize row: %v", res.ScanID, err)
	}
	if _, err := s.db.ExecContext(fctx,
		`UPDATE creators SET last_scan_at = ? WHERE id = ?`, nowRFC3339(), creatorID); err != nil {
		log.Printf("[scanner] scan %d: update last_scan_at: %v", res.ScanID, err)
	}

	s.publishDone(res)

	// Subscription auto-download matching (no-op while the enqueuer is nil).
	s.enqueueNewWorks(fctx, creatorID, newWorkIDs)
	res.NewWorkIDs = newWorkIDs
	return res
}

// writeFinalRow persists the terminal scan_runs columns.
func (s *Scanner) writeFinalRow(res Result) error {
	var lastErr any
	if res.LastError != nil {
		lastErr = *res.LastError
	}
	_, err := s.db.Exec(`
		UPDATE scan_runs SET status = ?, pages = ?, new_count = ?, updated_count = ?,
		       empty_pages = ?, completeness = ?, last_error = ?, finished_at = ?
		WHERE id = ?`,
		res.Status, res.Pages, res.NewCount, res.UpdatedCount, res.EmptyPages,
		res.Completeness, lastErr, nowRFC3339(), res.ScanID)
	if err != nil {
		return fmt.Errorf("scanner: finalize scan run %d: %w", res.ScanID, err)
	}
	return nil
}

// publishDone emits the state-class scan.done event.
func (s *Scanner) publishDone(res Result) {
	if s.bus == nil {
		return
	}
	s.bus.Publish(events.Event{
		Type: events.TypeScanDone,
		Data: events.ScanDone{
			ScanID:       res.ScanID,
			CreatorID:    res.CreatorID,
			Status:       res.Status,
			Pages:        res.Pages,
			NewCount:     res.NewCount,
			Completeness: res.Completeness,
			LastError:    res.LastError,
		},
	})
}

func seenIDs(seen map[int64]bool) []int64 {
	out := make([]int64, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	return out
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
