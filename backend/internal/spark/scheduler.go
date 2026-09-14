package spark

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"strings"
	"time"

	"douyin/backend/internal/events"
)

// Scheduler constants (docs/HUOHUA_EXECUTION_PLAN.md §6.4/§6.5).
const (
	tickInterval = 30 * time.Second
)

// Run services due sends every 30s until ctx is canceled. Manual triggers
// share the same mutual exclusion (beginSend), so a scheduled tick silently
// skips while a manual run is in flight.
func (s *Service) Run(ctx context.Context) {
	if n, err := s.store.ResetStuckSending(); err != nil {
		log.Printf("[spark] reset stuck sending: %v", err)
	} else if n > 0 {
		log.Printf("[spark] reset %d account(s) stranded in sending", n)
	}
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tick(ctx)
			// A tick that spent minutes sending lets the next tick fire
			// immediately on return; drain so we wait a fresh interval.
			select {
			case <-ticker.C:
			default:
			}
		}
	}
}

// tick is one scheduler pass: config gate -> window gate -> per-account
// due-target send (serial, never concurrent with manual runs).
func (s *Service) tick(ctx context.Context) {
	if !s.Configured() || !s.beginSend() {
		return
	}
	defer s.endSend()

	cfg, err := s.store.LoadSendConfig()
	if err != nil {
		log.Printf("[spark] load send config: %v", err)
		return
	}
	if !cfg.SendWindow.Enabled {
		return
	}
	now := s.NowLocal()
	if !inSendWindow(cfg, now) {
		return
	}
	accounts, err := s.store.ListAccounts()
	if err != nil {
		log.Printf("[spark] list accounts: %v", err)
		return
	}
	first := true
	for _, acct := range accounts {
		if ctx.Err() != nil {
			return
		}
		if !s.schedulableAccount(acct, now) {
			continue
		}
		due, _ := s.selectDueTargets(acct, cfg, now)
		if len(due) == 0 {
			continue
		}
		if !first {
			sleepBetween(ctx, cfg.SendStrategy.AccountStartDelaySecondsMin, cfg.SendStrategy.AccountStartDelaySecondsMax)
		}
		first = false
		if ctx.Err() != nil {
			return
		}
		log.Printf("[spark] scheduled run acct=%d (%s): %d due target(s)", acct.ID, acct.UniqueID, len(due))
		s.runAccountSend(ctx, acct, due, cfg, "scheduled")
	}
}

// schedulableAccount filters accounts for scheduled runs.
func (s *Service) schedulableAccount(acct Account, now time.Time) bool {
	if !acct.Enabled {
		return false
	}
	switch acct.Status {
	case "sending", "login_required":
		return false
	}
	if acct.CooldownUntil != "" {
		if until, err := time.Parse(time.RFC3339, acct.CooldownUntil); err == nil {
			if now.After(until) {
				return true
			}
			return false
		}
		// Unparseable cooldown stamp: treat as expired rather than blocking.
	}
	return true
}

// selectDueTargets is the Go translation of the upstream _select_due_targets
// (tasks.py:1830-1888): skip already-confirmed and already-failed targets,
// then send whatever passed its deterministic schedule time.
func (s *Service) selectDueTargets(acct Account, cfg SendConfig, now time.Time) (due, pending []string) {
	friends, err := s.store.ListFriends(acct.ID, nil, "")
	if err != nil {
		log.Printf("[spark] list friends acct=%d: %v", acct.ID, err)
		return nil, nil
	}
	var selected []string
	for _, f := range friends {
		if f.Selected {
			selected = append(selected, f.FriendKey)
		}
	}
	if len(selected) == 0 {
		return nil, nil
	}
	today := s.LocalDate(now)
	states, err := s.store.TodayFriendStates(acct.ID, today)
	if err != nil {
		log.Printf("[spark] today states acct=%d: %v", acct.ID, err)
		return nil, nil
	}
	for _, t := range selected {
		st := states[t]
		if st == "strong" || strings.HasPrefix(st, "failed") {
			// Already confirmed today, or already failed today (waits for a
			// manual retry or the next day) — §6.4.
			continue
		}
		scheduled := s.scheduledTimeFor(acct, cfg, t, now)
		if !now.Before(scheduled) {
			due = append(due, t)
		} else {
			pending = append(pending, t)
		}
	}
	return due, pending
}

// scheduledTimeFor derives a target's send time deterministically from
// sha256(localDate|accountUniqueID|target) — the same spread the upstream
// applies (tasks.py:1816-1827): the first 8 hex chars modulo the window
// width pick the minute offset inside the day's window.
func (s *Service) scheduledTimeFor(acct Account, cfg SendConfig, target string, now time.Time) time.Time {
	windowMinutes := (cfg.SendWindow.EndHour - cfg.SendWindow.StartHour) * 60
	if windowMinutes <= 0 {
		windowMinutes = 1
	}
	seed := fmt.Sprintf("%s|%s|%s", s.LocalDate(now), acct.UniqueID, target)
	sum := sha256.Sum256([]byte(seed))
	hex8 := hex.EncodeToString(sum[:4]) // 8 hex chars
	v, err := parseInt(hex8, 16)
	if err != nil {
		v = 0
	}
	offset := int(v) % windowMinutes
	start := time.Date(now.Year(), now.Month(), now.Day(), cfg.SendWindow.StartHour, 0, 0, 0, now.Location())
	return start.Add(time.Duration(offset) * time.Minute)
}

// parseInt is a tiny strconv-free helper (already imported crypto modules;
// strconv would be fine too but keeps imports tight).
func parseInt(s string, base int) (int64, error) {
	var n int64
	for _, c := range []byte(strings.ToLower(s)) {
		var d int64
		switch {
		case c >= '0' && c <= '9':
			d = int64(c - '0')
		case c >= 'a' && c <= 'f':
			d = int64(c-'a') + 10
		default:
			return 0, fmt.Errorf("spark: bad digit %q", c)
		}
		if int(d) >= base {
			return 0, fmt.Errorf("spark: digit %q out of base %d", c, base)
		}
		n = n*int64(base) + d
	}
	return n, nil
}

// publishLoginStatus is used by the API layer after a successful login export.
func (s *Service) publishLoginStatus(loggedIn bool, uniqueID string) {
	s.bus.Publish(events.Event{
		Type: events.TypeSparkLoginStatus,
		Data: events.SparkLoginStatus{LoggedIn: loggedIn, UniqueID: uniqueID},
	})
}
