package spark

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"douyin/backend/internal/config"
	"douyin/backend/internal/events"
	"douyin/backend/internal/settings"
)

// ErrNotConfigured / ErrSendBusy map to 503 / 409 in the API layer.
var (
	ErrNotConfigured = errors.New("spark not configured")
	ErrSendBusy      = errors.New("send already running")
)

// Service wires the engine client, the store and the event bus together and
// owns the send orchestration shared by manual triggers and the scheduler.
type Service struct {
	cfg     config.Settings
	client  *Client
	store   *Store
	bus     *events.Bus
	cookies *settings.Store // cookie file writer (nil in some tests)
	loc     *time.Location  // Asia/Shanghai

	sendMu  sync.Mutex
	sending bool
}

// New assembles the service. cookieStore may be nil in tests (cookie export
// then fails at the write step, not before).
func New(cfg config.Settings, handle *sql.DB, bus *events.Bus, cookieStore *settings.Store) *Service {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		// Fallback with a fixed +08:00 zone; time/tzdata is embedded in the
		// server binary so this path is defensive only.
		loc = time.FixedZone("Asia/Shanghai", 8*60*60)
	}
	return &Service{
		cfg:     cfg,
		client:  NewClient(cfg.SparkURL, cfg.SparkToken),
		store:   NewStore(handle),
		bus:     bus,
		cookies: cookieStore,
		loc:     loc,
	}
}

// Configured reports whether the spark surface is enabled (token present).
func (s *Service) Configured() bool { return strings.TrimSpace(s.cfg.SparkToken) != "" }

// Store exposes the store to the API layer.
func (s *Service) Store() *Store { return s.store }

// Client exposes the engine client (login proxy uses BaseURL only, but tests
// may want the client).
func (s *Service) Client() *Client { return s.client }

// BaseURL is the engine root for the login reverse proxy.
func (s *Service) BaseURL() string { return s.client.BaseURL() }

// LocalDate renders t in Asia/Shanghai as YYYY-MM-DD ("今日" key).
func (s *Service) LocalDate(t time.Time) string { return t.In(s.loc).Format("2006-01-02") }

// Location returns the service's local zone (scheduler window math).
func (s *Service) Location() *time.Location { return s.loc }

// NowLocal returns time.Now() in Asia/Shanghai.
func (s *Service) NowLocal() time.Time { return time.Now().In(s.loc) }

// beginSend/endSend implement the manual-vs-scheduled mutual exclusion
// (one send run at a time; second caller gets 409).
func (s *Service) beginSend() bool {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if s.sending {
		return false
	}
	s.sending = true
	return true
}

func (s *Service) endSend() {
	s.sendMu.Lock()
	s.sending = false
	s.sendMu.Unlock()
}

// ---------------------------------------------------------------- overview --

// EngineStatus is the engine health block of the overview payload.
type EngineStatus struct {
	OK          bool   `json:"ok"`
	Version     int    `json:"version,omitempty"`
	TaskRunning bool   `json:"task_running,omitempty"`
	Detail      string `json:"detail,omitempty"`
}

// WindowState reports the send window status for the UI.
type WindowState struct {
	Enabled  bool   `json:"enabled"`
	InWindow bool   `json:"in_window"`
	Now      string `json:"now"`
}

// Overview is GET /api/spark/overview.
type Overview struct {
	Engine   EngineStatus `json:"engine"`
	Accounts []Account    `json:"accounts"`
	Today    DayStats     `json:"today"`
	Window   WindowState  `json:"window"`
}

// Overview assembles the dashboard payload. A dead engine degrades to
// engine.ok=false — never a 500 (§T2.3 acceptance).
func (s *Service) Overview(ctx context.Context) Overview {
	out := Overview{Accounts: []Account{}}
	if s.Configured() {
		// Short probe: the dashboard polls this; never hang on a dead engine.
		pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		if h, err := s.client.Health(pctx); err == nil {
			out.Engine = EngineStatus{OK: true, Version: h.Version, TaskRunning: h.TaskRunning}
		} else {
			out.Engine = EngineStatus{OK: false, Detail: "engine unreachable"}
		}
		cancel()
	} else {
		out.Engine = EngineStatus{OK: false, Detail: "spark not configured"}
	}

	accounts, err := s.store.ListAccounts()
	if err == nil {
		out.Accounts = accounts
	}
	today := s.LocalDate(time.Now())
	for _, a := range out.Accounts {
		if st, err := s.store.TodayStats(a.ID, today); err == nil {
			out.Today.Strong += st.Strong
			out.Today.Weak += st.Weak
			out.Today.Failed += st.Failed
		}
	}

	cfg, err := s.store.LoadSendConfig()
	if err != nil {
		log.Printf("[spark] load send config for overview: %v", err)
	}
	now := s.NowLocal()
	out.Window = WindowState{
		Enabled:  cfg.SendWindow.Enabled,
		InWindow: inSendWindow(cfg, now),
		Now:      now.Format(time.RFC3339),
	}
	return out
}

// inSendWindow reports whether now is inside [startHour, endHour) plus the
// intervalMinutes grace period past EndHour (§6.4: outside the window +
// grace, no target is ever due).
func inSendWindow(cfg SendConfig, now time.Time) bool {
	if !cfg.SendWindow.Enabled {
		return false
	}
	start := time.Date(now.Year(), now.Month(), now.Day(), cfg.SendWindow.StartHour, 0, 0, 0, now.Location())
	end := time.Date(now.Year(), now.Month(), now.Day(), cfg.SendWindow.EndHour, 0, 0, 0, now.Location())
	grace := time.Duration(cfg.SendWindow.IntervalMinutes) * time.Minute
	return !now.Before(start) && !now.After(end.Add(grace))
}

// ------------------------------------------------------------------ friends --

// RefreshFriends pulls the friend list from the engine and swaps it into
// SQLite (selected flags preserved, new friends unselected).
func (s *Service) RefreshFriends(ctx context.Context, accountID int64) (total, newCount int, err error) {
	acct, err := s.store.GetAccount(accountID)
	if err != nil {
		return 0, 0, err
	}
	resp, err := s.client.RefreshFriends(ctx, acct.ProfileName)
	if err != nil {
		return 0, 0, err
	}
	total, newCount, err = s.store.ReplaceFriends(acct.ID, resp.Friends)
	if err != nil {
		return 0, 0, err
	}
	if err := s.store.SetAccountTimestamp(acct.ID, "last_friends_refresh_at", nowRFC3339()); err != nil {
		log.Printf("[spark] stamp friends refresh: %v", err)
	}
	s.bus.Publish(events.Event{
		Type: events.TypeSparkFriendsUpdated,
		Data: events.SparkFriendsUpdated{AccountID: acct.ID, Count: total},
	})
	return total, newCount, nil
}

// ------------------------------------------------------------------- sends --

// TriggerSend launches a manual run (async; progress via SSE).
func (s *Service) TriggerSend(mode string, accountIDs []int64) error {
	if !s.Configured() {
		return ErrNotConfigured
	}
	switch mode {
	case "now", "failed", "unsent":
	default:
		return fmt.Errorf("unknown mode %q (want now|failed|unsent)", mode)
	}
	if !s.beginSend() {
		return ErrSendBusy
	}
	go func() {
		defer s.endSend()
		s.runManualSend(context.Background(), mode, accountIDs)
	}()
	return nil
}

// runModeFor maps the trigger mode to the run_mode column value.
func runModeFor(mode string) string {
	switch mode {
	case "failed":
		return "manual_failed"
	case "unsent":
		return "manual_unsent"
	case "scheduled":
		return "scheduled"
	default:
		return "manual"
	}
}

// runManualSend executes the manual run across the selected accounts,
// honoring the configured account-start delays between accounts.
func (s *Service) runManualSend(ctx context.Context, mode string, accountIDs []int64) {
	cfg, err := s.store.LoadSendConfig()
	if err != nil {
		log.Printf("[spark] load send config: %v", err)
		return
	}
	accounts, err := s.store.ListAccounts()
	if err != nil {
		log.Printf("[spark] list accounts: %v", err)
		return
	}
	idSet := map[int64]struct{}{}
	for _, id := range accountIDs {
		idSet[id] = struct{}{}
	}
	today := s.LocalDate(time.Now())
	runMode := runModeFor(mode)

	first := true
	for _, acct := range accounts {
		if len(idSet) > 0 {
			if _, ok := idSet[acct.ID]; !ok {
				continue
			}
		}
		if !acct.Enabled {
			continue
		}
		targets, err := s.manualTargets(acct, cfg, mode, today)
		if err != nil {
			log.Printf("[spark] manual targets acct=%d: %v", acct.ID, err)
			continue
		}
		if len(targets) == 0 {
			continue
		}
		if !first && ctx.Err() == nil {
			sleepBetween(ctx, cfg.SendStrategy.AccountStartDelaySecondsMin, cfg.SendStrategy.AccountStartDelaySecondsMax)
		}
		first = false
		if ctx.Err() != nil {
			return
		}
		s.runAccountSend(ctx, acct, targets, cfg, runMode)
	}
}

// manualTargets computes the target list for a manual mode.
//
//	now    -> every selected friend (window and already-sent are ignored)
//	failed -> today's failed targets
//	unsent -> selected friends without any record today
func (s *Service) manualTargets(acct Account, cfg SendConfig, mode, today string) ([]string, error) {
	friends, err := s.store.ListFriends(acct.ID, nil, "")
	if err != nil {
		return nil, err
	}
	selected := map[string]struct{}{}
	for _, f := range friends {
		if f.Selected {
			selected[f.FriendKey] = struct{}{}
		}
	}
	if mode == "failed" {
		states, err := s.store.TodayFriendStates(acct.ID, today)
		if err != nil {
			return nil, err
		}
		var out []string
		for key, st := range states {
			if strings.HasPrefix(st, "failed") {
				out = append(out, key)
			}
		}
		return out, nil
	}
	var out []string
	if mode == "unsent" {
		states, err := s.store.TodayFriendStates(acct.ID, today)
		if err != nil {
			return nil, err
		}
		for key := range selected {
			if _, sent := states[key]; !sent {
				out = append(out, key)
			}
		}
		return out, nil
	}
	for key := range selected {
		out = append(out, key)
	}
	return out, nil
}

// runAccountSend executes one account's send run against the engine and
// persists every result. Called with the mutual exclusion already held.
func (s *Service) runAccountSend(ctx context.Context, acct Account, targets []string, cfg SendConfig, runMode string) {
	if len(targets) == 0 {
		return
	}
	today := s.LocalDate(time.Now())
	s.setAccountStatus(acct.ID, "sending", "")

	if cfg.SendStrategy.ShuffleTargets {
		rand.Shuffle(len(targets), func(i, j int) { targets[i], targets[j] = targets[j], targets[i] })
	}
	prev, err := s.store.LastStrongMessages(acct.ID, today)
	if err != nil {
		log.Printf("[spark] previous messages acct=%d: %v", acct.ID, err)
		prev = map[string]string{}
	}

	resp, err := s.client.RunSend(ctx, SendRunRequest{
		ProfileName:      acct.ProfileName,
		UniqueID:         acct.UniqueID,
		AccountName:      acct.Nickname,
		Targets:          targets,
		Config:           cfg.EnginePayload(),
		PreviousMessages: prev,
	})
	if err != nil {
		s.handleSendError(acct, targets, cfg, runMode, today, err)
		return
	}

	// Persist results; a target missing from the response (engine crash
	// mid-loop) becomes an explicit failed row so the day stays complete.
	sentAt := nowRFC3339()
	recs := make([]SendRecord, 0, len(targets))
	var counts DayStats
	seen := map[string]struct{}{}
	accountFailed := false
	detail := ""
	for _, r := range resp.Results {
		seen[r.Target] = struct{}{}
		if r.Category == "login_required" || r.Category == "account_error" {
			accountFailed = true
			detail = r.Detail
		}
		ts := r.SentAt
		if ts == "" {
			ts = sentAt
		}
		recs = append(recs, SendRecord{
			AccountID: acct.ID, FriendKey: r.Target, Message: r.Message,
			ConfirmState: r.State, Category: r.Category, Detail: r.Detail,
			RunMode: runMode, SentAt: ts, LocalDate: today,
		})
		switch r.State {
		case "strong":
			counts.Strong++
		case "weak":
			counts.Weak++
		default:
			counts.Failed++
		}
		s.bus.Publish(events.Event{
			Type: events.TypeSparkSendProgress,
			Data: events.SparkSendProgress{
				AccountID: acct.ID, Target: r.Target, State: r.State,
				Category: r.Category, Detail: r.Detail,
			},
		})
	}
	for _, t := range targets {
		if _, ok := seen[t]; ok {
			continue
		}
		recs = append(recs, SendRecord{
			AccountID: acct.ID, FriendKey: t, Message: "",
			ConfirmState: "failed", Category: "account_error",
			Detail: "no result returned by engine", RunMode: runMode,
			SentAt: sentAt, LocalDate: today,
		})
		counts.Failed++
		s.bus.Publish(events.Event{
			Type: events.TypeSparkSendProgress,
			Data: events.SparkSendProgress{
				AccountID: acct.ID, Target: t, State: "failed",
				Category: "account_error", Detail: "no result returned by engine",
			},
		})
	}
	if err := s.store.InsertSendRecords(recs); err != nil {
		log.Printf("[spark] persist send records acct=%d: %v", acct.ID, err)
	}
	if len(resp.AccountFailure) > 0 {
		accountFailed = true
		detail = fmt.Sprint(resp.AccountFailure["reason"])
		if detail == "" {
			detail = "engine reported account failure"
		}
	}
	if err := s.store.SetAccountTimestamp(acct.ID, "last_send_at", sentAt); err != nil {
		log.Printf("[spark] stamp last send: %v", err)
	}
	s.bus.Publish(events.Event{
		Type: events.TypeSparkSendFinished,
		Data: events.SparkSendFinished{
			AccountID: acct.ID, Strong: counts.Strong,
			Weak: counts.Weak, Failed: counts.Failed,
		},
	})

	if accountFailed {
		// One bump per run (not per target) — §6.5.
		s.finishAccountFailure(acct, cfg, detail)
		return
	}
	s.setAccountStatus(acct.ID, "idle", "")
}

// handleSendError records an engine-level failure (unreachable / hard error)
// for every target of the run and applies the account cooldown policy.
func (s *Service) handleSendError(acct Account, targets []string, cfg SendConfig, runMode, today string, err error) {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Busy() {
		// Engine busy: our own lock should prevent this; back off gracefully
		// without poisoning stats.
		log.Printf("[spark] engine busy, run skipped acct=%d", acct.ID)
		s.setAccountStatus(acct.ID, "idle", "engine busy, run skipped")
		return
	}
	detail := err.Error()
	recs := make([]SendRecord, 0, len(targets))
	for _, t := range targets {
		recs = append(recs, SendRecord{
			AccountID: acct.ID, FriendKey: t, Message: "",
			ConfirmState: "failed", Category: "account_error", Detail: detail,
			RunMode: runMode, SentAt: nowRFC3339(), LocalDate: today,
		})
		s.bus.Publish(events.Event{
			Type: events.TypeSparkSendProgress,
			Data: events.SparkSendProgress{
				AccountID: acct.ID, Target: t, State: "failed",
				Category: "account_error", Detail: detail,
			},
		})
	}
	if err := s.store.InsertSendRecords(recs); err != nil {
		log.Printf("[spark] persist error records acct=%d: %v", acct.ID, err)
	}
	s.bus.Publish(events.Event{
		Type: events.TypeSparkSendFinished,
		Data: events.SparkSendFinished{AccountID: acct.ID, Failed: len(targets)},
	})
	s.finishAccountFailure(acct, cfg, detail)
}

// finishAccountFailure applies the failure counter + cooldown policy and
// publishes the resulting status.
func (s *Service) finishAccountFailure(acct Account, cfg SendConfig, detail string) {
	cooled, err := s.store.RecordAccountFailure(
		acct.ID, s.LocalDate(time.Now()),
		cfg.AccountFailurePause.Attempts, cfg.AccountFailurePause.CooldownMinutes,
		s.NowLocal())
	if err != nil {
		log.Printf("[spark] record failure acct=%d: %v", acct.ID, err)
		s.setAccountStatus(acct.ID, "error", detail)
		return
	}
	if cooled {
		s.setAccountStatus(acct.ID, "cooldown", detail)
		return
	}
	s.setAccountStatus(acct.ID, "error", detail)
}

// setAccountStatus persists the status and publishes spark.account.status.
func (s *Service) setAccountStatus(accountID int64, status, detail string) {
	if err := s.store.SetAccountRuntime(accountID, status, detail); err != nil {
		log.Printf("[spark] set status acct=%d: %v", accountID, err)
	}
	s.bus.Publish(events.Event{
		Type: events.TypeSparkAccountStatus,
		Data: events.SparkAccountStatus{AccountID: accountID, Status: status, Detail: detail},
	})
}

// sleepBetween sleeps a random duration in [min,max] seconds (context-aware).
func sleepBetween(ctx context.Context, minS, maxS int) {
	lo, hi := minS, maxS
	if hi < lo {
		hi = lo
	}
	n := lo
	if hi > lo {
		n = lo + rand.Intn(hi-lo+1)
	}
	if n <= 0 {
		return
	}
	timer := time.NewTimer(time.Duration(n) * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// ------------------------------------------------------------ cookie export --

// ExportCookiesToArchive exports the account's login cookies from the engine
// and atomically writes <data_dir>/.cookie — the sidecar hot-reloads it on
// the next archived scan (the whole point of the unified login state).
func (s *Service) ExportCookiesToArchive(ctx context.Context, accountID int64) (int, error) {
	if !s.Configured() {
		return 0, ErrNotConfigured
	}
	acct, err := s.store.GetAccount(accountID)
	if err != nil {
		return 0, err
	}
	if s.cookies == nil {
		return 0, fmt.Errorf("spark: cookie writer unavailable")
	}
	exp, err := s.client.ExportCookies(ctx, acct.ProfileName)
	if err != nil {
		return 0, err
	}
	cookie := strings.TrimSpace(exp.Cookie)
	if !strings.Contains(cookie, "=") {
		return 0, fmt.Errorf("spark: engine returned an empty/invalid cookie header")
	}
	if err := s.cookies.WriteCookieFile(cookie); err != nil {
		return 0, err
	}
	return exp.CookieCount, nil
}

// cookieFilePath exposes where the cookie lands (for the API response/docs).
func (s *Service) cookieFilePath() string {
	if s.cookies == nil {
		return filepath.Join(s.cfg.DataDir, ".cookie")
	}
	return s.cookies.CookieFilePath()
}
