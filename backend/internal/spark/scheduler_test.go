package spark

import (
	"path/filepath"
	"testing"
	"time"

	"douyin/backend/internal/config"
	"douyin/backend/internal/db"
	"douyin/backend/internal/events"
)

// newTestService builds a Service over a temp SQLite (engine client never
// called in these tests).
func newTestService(t *testing.T) *Service {
	t.Helper()
	handle, err := db.Open(filepath.Join(t.TempDir(), "spark.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Migrate(handle, t.TempDir()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { handle.Close() })
	cfg := config.Settings{DataDir: t.TempDir(), SparkURL: "http://127.0.0.1:18788", SparkToken: "tok"}
	return New(cfg, handle, events.New(), nil)
}

func testConfig() SendConfig {
	return DefaultSendConfig() // window 10-18, interval 20
}

// scheduledTimeFor must be deterministic for the same seed inputs and always
// land inside the day's window (sha256(localDate|account|target)[:8] % width).
func TestScheduledTimeDeterministic(t *testing.T) {
	svc := newTestService(t)
	acct := Account{ID: 1, UniqueID: "demo"}
	cfg := testConfig()
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, svc.Location())

	a1 := svc.scheduledTimeFor(acct, cfg, "张三", now)
	a2 := svc.scheduledTimeFor(acct, cfg, "张三", now)
	if !a1.Equal(a2) {
		t.Fatalf("non-deterministic: %v vs %v", a1, a2)
	}

	start := time.Date(2026, 9, 14, 10, 0, 0, 0, svc.Location())
	end := time.Date(2026, 9, 14, 18, 0, 0, 0, svc.Location())
	if a1.Before(start) || a1.After(end.Add(-time.Minute)) {
		t.Fatalf("scheduled time %v outside [%v, %v)", a1, start, end)
	}
	if a1.Location() != svc.Location() || a1.Day() != 14 {
		t.Fatalf("scheduled time not local-day anchored: %v", a1)
	}

	// Different target → different seeds (spread actually varies).
	distinct := map[string]bool{}
	for _, target := range []string{"张三", "李四", "王五", "赵六", "钱七", "孙八", "周九", "吴十"} {
		st := svc.scheduledTimeFor(acct, cfg, target, now)
		distinct[st.Format("15:04")] = true
	}
	if len(distinct) < 3 {
		t.Fatalf("spread suspiciously narrow: %v", distinct)
	}
}

func TestInSendWindow(t *testing.T) {
	loc := time.FixedZone("Asia/Shanghai", 8*60*60)
	cfg := testConfig() // 10:00-18:00 + 20min grace, enabled

	cases := []struct {
		hour, minute int
		want         bool
	}{
		{9, 59, false},  // before start
		{10, 0, true},   // start boundary
		{12, 0, true},   // inside
		{18, 0, true},   // end boundary
		{18, 20, true},  // inside grace
		{18, 21, false}, // past grace
		{23, 0, false},  // far outside
	}
	for _, tc := range cases {
		now := time.Date(2026, 9, 14, tc.hour, tc.minute, 0, 0, loc)
		if got := inSendWindow(cfg, now); got != tc.want {
			t.Errorf("inSendWindow(%02d:%02d) = %v, want %v", tc.hour, tc.minute, got, tc.want)
		}
	}

	disabled := cfg
	disabled.SendWindow.Enabled = false
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, loc)
	if inSendWindow(disabled, now) {
		t.Fatalf("disabled window reported in-window")
	}
}

// selectDueTargets: strong/failed targets are skipped; the rest split by
// their deterministic schedule time vs now.
func TestSelectDueTargetsBranches(t *testing.T) {
	svc := newTestService(t)
	acct := mustCreateAccount(t, svc.Store(), "demo")

	friends := []FriendPair{
		{Key: "已发", DisplayName: "已发"},
		{Key: "已败", DisplayName: "已败"},
		{Key: "将发", DisplayName: "将发"},
		{Key: "待发", DisplayName: "待发"},
	}
	if _, _, err := svc.Store().ReplaceFriends(acct.ID, friends); err != nil {
		t.Fatalf("seed friends: %v", err)
	}
	var sel []FriendUpdate
	for _, f := range friends {
		sel = append(sel, FriendUpdate{Key: f.Key, Selected: true})
	}
	if err := svc.Store().PatchFriends(acct.ID, sel); err != nil {
		t.Fatalf("select all: %v", err)
	}

	today := "2026-09-14"
	records := []SendRecord{
		{AccountID: acct.ID, FriendKey: "已发", Message: "m", ConfirmState: "strong", RunMode: "scheduled", SentAt: "2026-09-14T04:00:00Z", LocalDate: today},
		{AccountID: acct.ID, FriendKey: "已败", Message: "m", ConfirmState: "failed", Category: "send_failed", RunMode: "scheduled", SentAt: "2026-09-14T04:01:00Z", LocalDate: today},
	}
	if err := svc.Store().InsertSendRecords(records); err != nil {
		t.Fatalf("seed records: %v", err)
	}

	cfg := testConfig()
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, svc.Location())
	due, pending := svc.selectDueTargets(acct, cfg, now)

	joined := map[string]bool{}
	for _, k := range due {
		joined[k] = true
	}
	for _, k := range pending {
		joined[k] = true
	}
	if joined["已发"] || joined["已败"] {
		t.Fatalf("strong/failed targets must be skipped: due=%v pending=%v", due, pending)
	}
	if len(due)+len(pending) != 2 {
		t.Fatalf("want exactly 2 live targets, due=%v pending=%v", due, pending)
	}
	for _, k := range due {
		st := svc.scheduledTimeFor(acct, cfg, k, now)
		if now.Before(st) {
			t.Fatalf("%s classified due but scheduled at %v > now", k, st)
		}
	}
	for _, k := range pending {
		st := svc.scheduledTimeFor(acct, cfg, k, now)
		if !now.Before(st) {
			t.Fatalf("%s classified pending but scheduled at %v <= now", k, st)
		}
	}
}

// schedulableAccount gates: disabled / sending / login_required / future
// cooldown are excluded; expired cooldown re-enters the rotation.
func TestSchedulableAccount(t *testing.T) {
	svc := newTestService(t)
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, svc.Location())

	disabled := Account{Enabled: false, Status: "idle"}
	if svc.schedulableAccount(disabled, now) {
		t.Fatalf("disabled account schedulable")
	}
	sending := Account{Enabled: true, Status: "sending"}
	if svc.schedulableAccount(sending, now) {
		t.Fatalf("sending account schedulable")
	}
	locked := Account{Enabled: true, Status: "login_required"}
	if svc.schedulableAccount(locked, now) {
		t.Fatalf("login_required account schedulable")
	}
	cooled := Account{Enabled: true, Status: "cooldown",
		CooldownUntil: now.Add(30 * time.Minute).Format(time.RFC3339)}
	if svc.schedulableAccount(cooled, now) {
		t.Fatalf("cooled account schedulable")
	}
	expired := Account{Enabled: true, Status: "cooldown",
		CooldownUntil: now.Add(-time.Minute).Format(time.RFC3339)}
	if !svc.schedulableAccount(expired, now) {
		t.Fatalf("expired cooldown still blocked")
	}
	idle := Account{Enabled: true, Status: "idle"}
	if !svc.schedulableAccount(idle, now) {
		t.Fatalf("idle account blocked")
	}
}
