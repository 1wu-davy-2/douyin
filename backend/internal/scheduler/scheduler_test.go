package scheduler

// Scheduler tests: due-service of never-run subscriptions, interval respect,
// the automatic upgrade to full scans (partial+gap or stale full scan), and
// next-run projection. Uses the real MockProvider for fast deterministic
// scans.

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"douyin/backend/internal/config"
	"douyin/backend/internal/db"
	"douyin/backend/internal/provider"
	"douyin/backend/internal/scanner"
	"douyin/backend/internal/settings"
)

func newFixture(t *testing.T) (*sql.DB, *Scheduler, int64) {
	t.Helper()
	handle, err := db.Open(filepath.Join(t.TempDir(), "sched.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Migrate(handle, t.TempDir()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { handle.Close() })

	store := settings.NewStore(handle, config.Settings{DataDir: t.TempDir()})
	resolver := provider.NewResolver(config.Settings{Mock: true}, nil, store)
	sc := scanner.New(context.Background(), resolver, handle, nil, store)

	if _, err := handle.Exec(
		`INSERT INTO creators (sec_uid, nickname, profile_url, created_at) VALUES ('MS4wLjABAAAAsched1', 'Sched博主', 'u', ?)`,
		time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	var creatorID int64
	if err := handle.QueryRow(`SELECT id FROM creators WHERE sec_uid = 'MS4wLjABAAAAsched1'`).Scan(&creatorID); err != nil {
		t.Fatal(err)
	}

	s := New(handle, sc, store)
	s.tick = 10 * time.Millisecond
	s.stagger = 0
	return handle, s, creatorID
}

// A subscription that never ran is due immediately: one tick must produce a
// scheduled scan run (finished, not just created) and refresh the
// subscription's last_run_at.
func TestSchedulerServicesDueSubscription(t *testing.T) {
	fdb, sched, creatorID := newFixture(t)
	if _, err := fdb.Exec(`
		INSERT INTO subscriptions (target_type, creator_id, interval_minutes, auto_download, quality, enabled, created_at)
		VALUES ('creator', ?, 60, 0, '1080p', 1, ?)`, creatorID, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		sched.Run(ctx)
		close(done)
	}()

	var status, trigger string
	var lastRun sql.NullString
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		// last_run_at is written after the scheduler's ScanSync returns, i.e.
		// after the scan fully finalized - waiting for it avoids racing the
		// context cancellation with the run's bookkeeping.
		err := fdb.QueryRow(`SELECT last_run_at FROM subscriptions`).Scan(&lastRun)
		if err == nil && lastRun.Valid {
			err = fdb.QueryRow(`
				SELECT status, "trigger" FROM scan_runs WHERE creator_id = ? ORDER BY id DESC LIMIT 1`,
				creatorID).Scan(&status, &trigger)
			if err == nil {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done

	if status != scanner.StatusSucceeded || trigger != scanner.TriggerScheduled {
		t.Fatalf("scan run = %s/%s, want succeeded/scheduled", status, trigger)
	}
	if !lastRun.Valid {
		t.Fatal("subscription last_run_at not refreshed")
	}
}

// Reconciliation upgrades: last scan partial with a completeness gap above the
// threshold -> the scheduled run is a FULL scan. A stale (>7d) successful full
// scan also upgrades; a fresh one does not.
func TestShouldUpgradeToFull(t *testing.T) {
	fdb, sched, creatorID := newFixture(t)
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339)
	old := time.Now().UTC().Add(-8 * 24 * time.Hour).Format(time.RFC3339)
	reset := func() { t.Helper(); fdb.Exec(`DELETE FROM scan_runs`) }
	insert := func(full int, status string, completeness int, finished string) {
		t.Helper()
		if _, err := fdb.Exec(`
			INSERT INTO scan_runs (creator_id, "trigger", full, status, completeness, started_at, finished_at)
			VALUES (?, 'manual', ?, ?, ?, ?, ?)`, creatorID, full, status, completeness, finished, finished); err != nil {
			t.Fatal(err)
		}
	}

	// No scans at all -> full.
	reset()
	if !sched.shouldUpgradeToFull(ctx, creatorID) {
		t.Fatal("creator without any scan should upgrade to full")
	}

	// Partial tail with a gap above the threshold -> full.
	reset()
	insert(0, "partial", 50, now)
	if !sched.shouldUpgradeToFull(ctx, creatorID) {
		t.Fatal("partial scan with gap 50 should upgrade to full")
	}

	// Recent successful full scan -> incremental.
	reset()
	insert(1, "succeeded", 0, now)
	if sched.shouldUpgradeToFull(ctx, creatorID) {
		t.Fatal("recent successful full scan should stay incremental")
	}

	// Only a successful full scan from 8 days ago -> full (7-day rule).
	reset()
	insert(1, "succeeded", 0, old)
	if !sched.shouldUpgradeToFull(ctx, creatorID) {
		t.Fatal("full scan older than 7 days should upgrade")
	}

	// Latest run partial with a small gap, but a fresh full scan exists ->
	// incremental (the gap does not exceed the threshold).
	reset()
	insert(1, "succeeded", 0, now)
	insert(0, "partial", 2, now)
	if sched.shouldUpgradeToFull(ctx, creatorID) {
		t.Fatal("partial scan with gap 2 and fresh full scan should stay incremental")
	}
}

// Due logic: never-run subscriptions are due; subscriptions inside their
// (jittered) interval are not; overdue ones are.
func TestDueLogic(t *testing.T) {
	sub := subscriptionRow{ID: 7, IntervalMinutes: 60}
	if !due(sub, time.Now()) {
		t.Fatal("never-run subscription must be due")
	}

	sub.LastRunAt = sql.NullString{String: time.Now().UTC().Add(-30 * time.Minute).Format(time.RFC3339), Valid: true}
	if due(sub, time.Now()) {
		t.Fatal("subscription at half interval must not be due (jitter maxes at +20%)")
	}

	sub.LastRunAt = sql.NullString{String: time.Now().UTC().Add(-90 * time.Minute).Format(time.RFC3339), Valid: true}
	if !due(sub, time.Now()) {
		t.Fatal("subscription past interval+jitter must be due")
	}
}

// NextRunAt projects the same jittered schedule the tick loop applies.
func TestNextRunAtProjection(t *testing.T) {
	last := "2026-01-01T00:00:00Z"
	created := "2025-12-31T00:00:00Z"

	n1 := NextRunAt(1, 60, &last, &created)
	expected := jitterDue(1, 60, last)
	if n1 == nil || *n1 != expected {
		t.Fatalf("NextRunAt = %v, want %v (last_run + interval * jitter)", n1, expected)
	}
	if n2 := NextRunAt(1, 60, &last, &created); *n1 != *n2 {
		t.Fatalf("NextRunAt not deterministic: %v vs %v", n1, n2)
	}

	// The projection stays within interval +- 20%.
	lastTime, _ := time.Parse(time.RFC3339, last)
	got, _ := time.Parse(time.RFC3339, *n1)
	delta := got.Sub(lastTime)
	if delta < 48*time.Minute || delta > 72*time.Minute {
		t.Fatalf("next run delta = %s, want between 48m and 72m", delta)
	}

	// Never-run falls back to created_at; no timestamps -> nil.
	if n3 := NextRunAt(1, 60, nil, &created); n3 == nil {
		t.Fatal("NextRunAt with nil last_run_at should project from created_at")
	}
	if n4 := NextRunAt(1, 60, nil, nil); n4 != nil {
		t.Fatal("NextRunAt without any timestamp should return nil")
	}
}

// jitterDue computes the expected projection the same way the scheduler does.
func jitterDue(subID int64, intervalMinutes int, lastRun string) string {
	t, _ := time.Parse(time.RFC3339, lastRun)
	interval := time.Duration(intervalMinutes) * time.Minute
	return t.Add(time.Duration(float64(interval) * jitterFactor(subID, lastRun))).UTC().Format(time.RFC3339)
}

// The deterministic interval jitter stays within the documented +-20%.
func TestJitterFactorBounds(t *testing.T) {
	for id := int64(1); id < 500; id++ {
		f := jitterFactor(id, "2026-01-01T00:00:00Z")
		if f < 0.8 || f >= 1.2 {
			t.Fatalf("jitterFactor(%d) = %f outside [0.8, 1.2)", id, f)
		}
	}
}
