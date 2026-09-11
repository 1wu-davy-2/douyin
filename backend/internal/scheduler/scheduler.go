// Package scheduler implements the subscription monitor (stage 4): a single
// goroutine ticks every 30s, finds enabled subscriptions whose interval has
// elapsed and runs incremental creator scans for them.
//
// Reconciliation upgrades (the legacy system never backfilled):
//   - if the creator's last scan ended partial with completeness above the
//     gap threshold, the scheduled run is upgraded to a FULL scan;
//   - if the last successful full scan is older than 7 days (or none ever
//     succeeded), the run is upgraded to FULL as well.
//
// The scanner's global single-flight serializes concurrent runs; a small
// random stagger plus a deterministic per-subscription interval jitter
// (+-20%, stable between ticks) spreads out subscriptions falling due at the
// same time.
package scheduler

import (
	"context"
	"database/sql"
	"fmt"
	"hash/fnv"
	"log"
	"math/rand"
	"time"

	"douyin/backend/internal/scanner"
	"douyin/backend/internal/settings"
)

// fullScanMaxAge is how long a successful full scan is trusted before the
// next scheduled run upgrades itself to full.
const fullScanMaxAge = 7 * 24 * time.Hour

// ScanService is the scanner surface the scheduler depends on
// (*scanner.Scanner implements it).
type ScanService interface {
	ScanSync(ctx context.Context, creatorID int64, full bool, trigger string) (scanner.Result, error)
}

// subscriptionRow is one enabled subscription read for a tick.
type subscriptionRow struct {
	ID              int64
	TargetType      string
	CreatorID       int64
	CollectionID    sql.NullInt64
	IntervalMinutes int
	LastRunAt       sql.NullString
	CreatedAt       string
}

// Scheduler drives the subscription scans.
type Scheduler struct {
	db      *sql.DB
	scans   ScanService
	store   *settings.Store
	tick    time.Duration // tick interval (30s in production)
	stagger time.Duration // max random pre-scan stagger (0 disables)
}

// New wires the scheduler with production timing.
func New(db *sql.DB, scans ScanService, store *settings.Store) *Scheduler {
	return &Scheduler{db: db, scans: scans, store: store, tick: 30 * time.Second, stagger: 5 * time.Second}
}

// Run blocks until ctx is canceled, servicing due subscriptions.
func (s *Scheduler) Run(ctx context.Context) {
	// One immediate pass so subscriptions become active without waiting a
	// full tick after startup.
	s.tickOnce(ctx)
	t := time.NewTicker(s.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.tickOnce(ctx)
		}
	}
}

// tickOnce services every due subscription once.
func (s *Scheduler) tickOnce(ctx context.Context) {
	subs, err := s.loadSubscriptions(ctx)
	if err != nil {
		log.Printf("[scheduler] load subscriptions: %v", err)
		return
	}
	now := time.Now()
	for _, sub := range subs {
		if ctx.Err() != nil {
			return
		}
		if !due(sub, now) {
			continue
		}
		// Stagger simultaneous due dates; the scanner's single-flight would
		// serialize them anyway, this just avoids bursty provider traffic.
		if s.stagger > 0 && len(subs) > 1 {
			d := time.Duration(rand.Int63n(int64(s.stagger) + 1))
			if err := sleepCtx(ctx, d); err != nil {
				return
			}
		}
		s.service(ctx, sub)
	}
}

// loadSubscriptions reads all enabled subscriptions.
func (s *Scheduler) loadSubscriptions(ctx context.Context) ([]subscriptionRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, target_type, creator_id, collection_id, interval_minutes, last_run_at, created_at
		FROM subscriptions WHERE enabled = 1 ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("scheduler: query subscriptions: %w", err)
	}
	defer rows.Close()
	var out []subscriptionRow
	for rows.Next() {
		var r subscriptionRow
		if err := rows.Scan(&r.ID, &r.TargetType, &r.CreatorID, &r.CollectionID,
			&r.IntervalMinutes, &r.LastRunAt, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("scheduler: scan subscription: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// due reports whether the subscription's (jittered) interval has elapsed.
// Subscriptions that never ran are due immediately.
func due(sub subscriptionRow, now time.Time) bool {
	if !sub.LastRunAt.Valid {
		return true
	}
	last, err := time.Parse(time.RFC3339, sub.LastRunAt.String)
	if err != nil {
		return true
	}
	interval := time.Duration(sub.IntervalMinutes) * time.Minute
	if interval <= 0 {
		interval = time.Minute
	}
	next := last.Add(time.Duration(float64(interval) * jitterFactor(sub.ID, sub.LastRunAt.String)))
	return !now.Before(next)
}

// jitterFactor derives a deterministic per-(subscription, last run) factor in
// [0.8, 1.2): the +-20% interval jitter. Derived from stable inputs so the
// projected next run does not re-roll between ticks.
func jitterFactor(subID int64, salt string) float64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "%d|%s", subID, salt)
	return 0.8 + 0.4*float64(h.Sum64()%1000)/1000.0
}

// NextRunAt projects the subscription's next due time for the API view.
// Never-run subscriptions are projected from created_at. Returns nil when the
// inputs are unusable.
func NextRunAt(subID int64, intervalMinutes int, lastRunAt, createdAt *string) *string {
	interval := time.Duration(intervalMinutes) * time.Minute
	if interval <= 0 {
		return nil
	}
	base, salt := "", ""
	switch {
	case lastRunAt != nil && *lastRunAt != "":
		base, salt = *lastRunAt, *lastRunAt
	case createdAt != nil && *createdAt != "":
		base, salt = *createdAt, *createdAt
	default:
		return nil
	}
	t, err := time.Parse(time.RFC3339, base)
	if err != nil {
		return nil
	}
	out := t.Add(time.Duration(float64(interval) * jitterFactor(subID, salt))).UTC().Format(time.RFC3339)
	return &out
}

// service runs one due subscription: scan (upgraded to full when
// reconciliation demands it), refresh last_run_at, log the outcome. New-work
// auto-download is matched by the scanner at finalize time, so collection
// targets need no special handling here.
func (s *Scheduler) service(ctx context.Context, sub subscriptionRow) {
	full := s.shouldUpgradeToFull(ctx, sub.CreatorID)
	res, err := s.scans.ScanSync(ctx, sub.CreatorID, full, scanner.TriggerScheduled)

	// last_run_at is refreshed even on failure so a broken creator is not
	// retried every 30s tick, only at the next interval.
	if _, uerr := s.db.ExecContext(ctx,
		`UPDATE subscriptions SET last_run_at = ? WHERE id = ?`, nowRFC3339(), sub.ID); uerr != nil {
		log.Printf("[scheduler] subscription %d: update last_run_at: %v", sub.ID, uerr)
	}

	if err != nil {
		if ctx.Err() != nil {
			return // shutting down
		}
		log.Printf("[scheduler] subscription %d: creator %d scan failed: %v", sub.ID, sub.CreatorID, err)
		return
	}

	log.Printf("[scheduler] subscription %d: creator %d scan %d finished: status=%s pages=%d new=%d updated=%d empty=%d completeness=%d full=%v",
		sub.ID, sub.CreatorID, res.ScanID, res.Status, res.Pages, res.NewCount,
		res.UpdatedCount, res.EmptyPages, res.Completeness, full)

	// Partial results would feed the notification sender (settings stage);
	// the notify hook is intentionally left empty here.
	if res.Status == scanner.StatusPartial {
		log.Printf("[scheduler] subscription %d: creator %d scan partial (completeness=%d) - notify hook reserved",
			sub.ID, sub.CreatorID, res.Completeness)
	}
}

// shouldUpgradeToFull decides whether this scheduled run must be a full scan:
// the last scan ended partial with a completeness gap above the threshold, or
// the last successful full scan is older than fullScanMaxAge (or none ever).
func (s *Scheduler) shouldUpgradeToFull(ctx context.Context, creatorID int64) bool {
	threshold := 5
	if s.store != nil {
		if v, err := s.store.View(ctx); err == nil {
			threshold = v.CompletenessGapThreshold
		}
	}

	var status string
	var completeness int
	err := s.db.QueryRowContext(ctx,
		`SELECT status, completeness FROM scan_runs WHERE creator_id = ? ORDER BY id DESC LIMIT 1`,
		creatorID).Scan(&status, &completeness)
	if err == nil && status == scanner.StatusPartial && completeness > threshold {
		return true
	}

	var lastFull sql.NullString
	err = s.db.QueryRowContext(ctx,
		`SELECT MAX(finished_at) FROM scan_runs WHERE creator_id = ? AND full = 1 AND status = ?`,
		creatorID, scanner.StatusSucceeded).Scan(&lastFull)
	if err != nil || !lastFull.Valid {
		return true // never had a successful full scan
	}
	t, perr := time.Parse(time.RFC3339, lastFull.String)
	if perr != nil {
		return true
	}
	return time.Since(t) > fullScanMaxAge
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
