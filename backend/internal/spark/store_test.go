package spark

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"douyin/backend/internal/db"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	handle, err := db.Open(filepath.Join(t.TempDir(), "spark.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Migrate(handle, t.TempDir()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { handle.Close() })
	return NewStore(handle)
}

func mustCreateAccount(t *testing.T, s *Store, uniqueID string) Account {
	t.Helper()
	a, err := s.CreateAccount(uniqueID, "", "昵称-"+uniqueID, "uid-"+uniqueID)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	return a
}

func TestAccountCRUD(t *testing.T) {
	s := newTestStore(t)
	a := mustCreateAccount(t, s, "demo")

	if a.ID == 0 || a.ProfileName != "uid-demo" || !a.Enabled || a.Status != "idle" {
		t.Fatalf("unexpected created account: %+v", a)
	}

	// Duplicate unique_id -> ErrDuplicateAccount.
	if _, err := s.CreateAccount("demo", "", "", "uid-demo"); !errors.Is(err, ErrDuplicateAccount) {
		t.Fatalf("duplicate create err = %v, want ErrDuplicateAccount", err)
	}

	// Partial update.
	disabled := false
	nick := "新昵称"
	got, err := s.UpdateAccount(a.ID, AccountPatch{Enabled: &disabled, Nickname: &nick})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if got.Enabled || got.Nickname != "新昵称" {
		t.Fatalf("update not applied: %+v", got)
	}

	list, err := s.ListAccounts()
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %v, %v", list, err)
	}

	if err := s.DeleteAccount(a.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetAccount(a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete = %v, want ErrNotFound", err)
	}
	if err := s.DeleteAccount(a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double delete = %v, want ErrNotFound", err)
	}
}

func TestReplaceFriendsPreservesSelection(t *testing.T) {
	s := newTestStore(t)
	a := mustCreateAccount(t, s, "demo")

	total, fresh, err := s.ReplaceFriends(a.ID, []FriendPair{
		{Key: "张三", DisplayName: "张三"},
		{Key: "李四", DisplayName: "李四"},
		{Key: "王五", DisplayName: "王五"},
	})
	if err != nil || total != 3 || fresh != 3 {
		t.Fatalf("first replace = %d/%d, %v", total, fresh, err)
	}

	// User selects 李四.
	if err := s.PatchFriends(a.ID, []FriendUpdate{{Key: "李四", Selected: true}}); err != nil {
		t.Fatalf("patch: %v", err)
	}

	// Re-refresh drops 张三, adds 赵六; 李四 must stay selected, 王五/赵六 not.
	total, fresh, err = s.ReplaceFriends(a.ID, []FriendPair{
		{Key: "李四", DisplayName: "李四"},
		{Key: "王五", DisplayName: "王五"},
		{Key: "赵六", DisplayName: "赵六"},
	})
	if err != nil || total != 3 || fresh != 1 {
		t.Fatalf("second replace = %d/%d, %v", total, fresh, err)
	}
	friends, err := s.ListFriends(a.ID, nil, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	selected := map[string]bool{}
	for _, f := range friends {
		selected[f.FriendKey] = f.Selected
	}
	if !selected["李四"] || selected["王五"] || selected["赵六"] {
		t.Fatalf("selection not preserved: %v", selected)
	}
	if _, exists := selected["张三"]; exists {
		t.Fatalf("dropped friend still present")
	}

	// Selected filter.
	sel := true
	onlySel, err := s.ListFriends(a.ID, &sel, "")
	if err != nil || len(onlySel) != 1 || onlySel[0].FriendKey != "李四" {
		t.Fatalf("selected filter = %v, %v", onlySel, err)
	}

	// Empty engine snapshot is rejected (guards against wiping the list on a
	// botched scrape).
	if _, _, err := s.ReplaceFriends(a.ID, nil); !errors.Is(err, ErrInvalidFriendList) {
		t.Fatalf("empty replace err = %v, want ErrInvalidFriendList", err)
	}
}

func TestTodayStatsAndFriendStates(t *testing.T) {
	s := newTestStore(t)
	a := mustCreateAccount(t, s, "demo")
	today := "2026-09-14"

	recs := []SendRecord{
		{AccountID: a.ID, FriendKey: "张三", Message: "m1", ConfirmState: "strong", RunMode: "scheduled", SentAt: "2026-09-14T04:00:00Z", LocalDate: today},
		{AccountID: a.ID, FriendKey: "李四", Message: "m2", ConfirmState: "failed", Category: "send_failed", Detail: "boom", RunMode: "scheduled", SentAt: "2026-09-14T04:01:00Z", LocalDate: today},
		{AccountID: a.ID, FriendKey: "李四", Message: "m3", ConfirmState: "failed", Category: "send_failed", Detail: "boom2", RunMode: "manual_failed", SentAt: "2026-09-14T05:00:00Z", LocalDate: today},
		// Yesterday's rows must not leak into today's stats.
		{AccountID: a.ID, FriendKey: "王五", Message: "m0", ConfirmState: "strong", RunMode: "scheduled", SentAt: "2026-09-13T04:00:00Z", LocalDate: "2026-09-13"},
	}
	if err := s.InsertSendRecords(recs); err != nil {
		t.Fatalf("insert: %v", err)
	}

	st, err := s.TodayStats(a.ID, today)
	if err != nil || st.Strong != 1 || st.Failed != 2 || st.Weak != 0 {
		t.Fatalf("stats = %+v, %v", st, err)
	}

	states, err := s.TodayFriendStates(a.ID, today)
	if err != nil {
		t.Fatalf("states: %v", err)
	}
	if states["张三"] != "strong" {
		t.Fatalf("张三 state = %q, want strong", states["张三"])
	}
	if states["李四"] != "failed:send_failed" {
		t.Fatalf("李四 state = %q, want failed:send_failed", states["李四"])
	}
	if _, ok := states["王五"]; ok {
		t.Fatalf("yesterday leaked into today")
	}

	// Strong is sticky: a later failed retry must not downgrade the day.
	if err := s.InsertSendRecords([]SendRecord{{
		AccountID: a.ID, FriendKey: "张三", Message: "m1b", ConfirmState: "failed",
		Category: "send_failed", RunMode: "manual_failed",
		SentAt: "2026-09-14T06:00:00Z", LocalDate: today,
	}}); err != nil {
		t.Fatalf("insert retry: %v", err)
	}
	states, _ = s.TodayFriendStates(a.ID, today)
	if states["张三"] != "strong" {
		t.Fatalf("strong downgraded by later failure: %q", states["张三"])
	}

	// previous_messages seed = newest strong message per friend.
	prev, err := s.LastStrongMessages(a.ID, today)
	if err != nil || prev["张三"] != "m1" {
		t.Fatalf("last strong messages = %v, %v", prev, err)
	}
}

func TestRecordAccountFailureCooldown(t *testing.T) {
	s := newTestStore(t)
	a := mustCreateAccount(t, s, "demo")
	today := "2026-09-14"
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

	for i := 1; i <= 2; i++ {
		cooled, err := s.RecordAccountFailure(a.ID, today, 3, 60, now)
		if err != nil || cooled {
			t.Fatalf("failure %d: cooled=%v err=%v", i, cooled, err)
		}
	}
	cooled, err := s.RecordAccountFailure(a.ID, today, 3, 60, now)
	if err != nil || !cooled {
		t.Fatalf("third failure: cooled=%v err=%v", cooled, err)
	}
	a, err = s.GetAccount(a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if a.Status != "cooldown" || a.CooldownUntil == "" {
		t.Fatalf("cooldown not applied: %+v", a)
	}
	until, err := time.Parse(time.RFC3339, a.CooldownUntil)
	if err != nil || !until.Equal(now.Add(60*time.Minute)) {
		t.Fatalf("cooldown_until = %v (%v), want %v", a.CooldownUntil, err, now.Add(60*time.Minute))
	}
	if a.FailureCountToday == "" {
		t.Fatalf("failure counter not persisted")
	}

	// New day: counter keeps history but a fresh key starts from zero.
	cooled, err = s.RecordAccountFailure(a.ID, "2026-09-15", 3, 60, now.Add(24*time.Hour))
	if err != nil || cooled {
		t.Fatalf("new-day failure: cooled=%v err=%v", cooled, err)
	}
}

func TestListSendRecordsCursor(t *testing.T) {
	s := newTestStore(t)
	a := mustCreateAccount(t, s, "demo")
	b := mustCreateAccount(t, s, "other")

	var recs []SendRecord
	for i := 0; i < 5; i++ {
		recs = append(recs, SendRecord{
			AccountID: a.ID, FriendKey: fmt.Sprintf("好友%d", i), Message: "m",
			ConfirmState: "strong", RunMode: "scheduled",
			SentAt: "2026-09-14T04:00:00Z", LocalDate: "2026-09-14",
		})
	}
	recs = append(recs, SendRecord{
		AccountID: b.ID, FriendKey: "别的号", Message: "m", ConfirmState: "strong",
		RunMode: "scheduled", SentAt: "2026-09-14T04:00:00Z", LocalDate: "2026-09-14",
	})
	if err := s.InsertSendRecords(recs); err != nil {
		t.Fatalf("insert: %v", err)
	}

	items, next, err := s.ListSendRecords(nil, 0, 3)
	if err != nil || len(items) != 3 || next == 0 {
		t.Fatalf("page1 = %d items next=%d err=%v", len(items), next, err)
	}
	if items[0].FriendKey != "别的号" || items[0].AccountLabel == "" {
		t.Fatalf("newest-first/label wrong: %+v", items[0])
	}
	items2, next2, err := s.ListSendRecords(nil, next, 3)
	if err != nil || len(items2) != 3 || next2 != 0 {
		t.Fatalf("page2 = %d items next=%d err=%v (last page must end the cursor chain)", len(items2), next2, err)
	}

	// Account filter.
	id := a.ID
	filtered, _, err := s.ListSendRecords(&id, 0, 100)
	if err != nil || len(filtered) != 5 {
		t.Fatalf("account filter = %d items err=%v", len(filtered), err)
	}
}

func TestSendConfigRoundtripAndClamps(t *testing.T) {
	s := newTestStore(t)

	def, err := s.LoadSendConfig()
	if err != nil {
		t.Fatalf("load default: %v", err)
	}
	if def.SendWindow.StartHour != 10 || def.SendStrategy.MessageIntervalSecondsMin != 25 {
		t.Fatalf("defaults wrong: %+v", def)
	}

	// Hostile values get clamped to the safe floors.
	def.SendStrategy.MessageIntervalSecondsMin = 1
	def.SendStrategy.MessageIntervalSecondsMax = 0
	def.SendWindow.EndHour = 3 // < start hour
	if err := s.SaveSendConfig(def); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := s.LoadSendConfig()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got = got.Normalize()
	if got.SendStrategy.MessageIntervalSecondsMin < 25 {
		t.Fatalf("interval floor not enforced: %+v", got.SendStrategy)
	}
	if got.SendStrategy.MessageIntervalSecondsMax < got.SendStrategy.MessageIntervalSecondsMin {
		t.Fatalf("max < min after normalize")
	}
	if got.SendWindow.EndHour != 18 || got.SendWindow.StartHour != 10 {
		t.Fatalf("window clamp not applied: %+v", got.SendWindow)
	}

	// Corrupt JSON falls back to defaults.
	if _, err := s.handle.Exec(`UPDATE spark_settings SET value = '{oops' WHERE key = 'send_config'`); err != nil {
		t.Fatalf("corrupt: %v", err)
	}
	fallback, err := s.LoadSendConfig()
	if err != nil {
		t.Fatalf("fallback load: %v", err)
	}
	if fallback.SendWindow.StartHour != 10 {
		t.Fatalf("corrupt config did not fall back to defaults: %+v", fallback)
	}
}

func TestResetStuckSending(t *testing.T) {
	s := newTestStore(t)
	a := mustCreateAccount(t, s, "demo")
	if err := s.SetAccountRuntime(a.ID, "sending", ""); err != nil {
		t.Fatalf("set sending: %v", err)
	}
	n, err := s.ResetStuckSending()
	if err != nil || n != 1 {
		t.Fatalf("reset = %d, %v", n, err)
	}
	a, _ = s.GetAccount(a.ID)
	if a.Status != "idle" {
		t.Fatalf("status = %q, want idle", a.Status)
	}
}
