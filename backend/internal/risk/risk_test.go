package risk

import (
	"errors"
	"testing"

	"douyin/backend/internal/events"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		text   string
		reason string
		isRisk bool
	}{
		// F2 upstream 403 on the post list (observed 2026-09-15).
		{`sidecar /posts: HTTP 502: HTTP状态码错误： Status Code: 403`, ReasonRisk403, true},
		{`sidecar /work: HTTP 502: HTTP状态码错误： Status Code: 403`, ReasonRisk403, true},
		// Empty payload shells the sidecar rejects as risk/expired-cookie.
		{`sidecar /posts: HTTP 502: posts: upstream returned no aweme_list (risk control or expired cookie?)`, ReasonEmptyShell, true},
		{`sidecar /work: HTTP 502: work: upstream returned no aweme_detail (risk control or expired cookie?)`, ReasonEmptyShell, true},
		// No cookie at all.
		{`sidecar /posts: HTTP 503: cookie not configured`, ReasonNoCookie, true},
		// The sidecar's OWN token guard 403 is infra, never douyin risk.
		{`sidecar /posts: HTTP 403: invalid or missing sidecar token`, "", false},
		{`sidecar /health: HTTP 403: invalid sidecar token (foreign sidecar on this port?)`, "", false},
		// Plain infra errors are not risk.
		{`sidecar /posts: process exited during startup: exit status 1`, "", false},
		{`sidecar /posts: dial tcp 127.0.0.1:18787: connectex: connection refused`, "", false},
		{`sidecar /posts: context deadline exceeded`, "", false},
	}
	for _, c := range cases {
		reason, isRisk := Classify(c.text)
		if isRisk != c.isRisk || reason != c.reason {
			t.Errorf("Classify(%q) = (%q, %v), want (%q, %v)", c.text, reason, isRisk, c.reason, c.isRisk)
		}
	}
}

func TestTrackerBlocksAfterThreshold(t *testing.T) {
	tr := New(nil, nil)
	err403 := errors.New(`sidecar /posts: HTTP 502: HTTP状态码错误： Status Code: 403`)

	tr.RecordFailure("/posts", err403)
	tr.RecordFailure("/posts", err403)
	if blocked, _, _ := tr.State(); blocked {
		t.Fatal("must not block below the threshold")
	}
	tr.RecordFailure("/posts", err403)
	blocked, reason, since := tr.State()
	if !blocked || reason != ReasonRisk403 || since == "" {
		t.Fatalf("expected blocked with reason and since, got %v %q %q", blocked, reason, since)
	}
	// Non-risk failures neither count nor reset.
	tr.RecordFailure("/posts", errors.New("sidecar /posts: context deadline exceeded"))
	if blocked, _, _ := tr.State(); !blocked {
		t.Fatal("non-risk failure must not change block state")
	}
}

func TestProfileSuccessDoesNotReset(t *testing.T) {
	tr := New(nil, nil)
	err403 := errors.New(`sidecar /posts: HTTP 502: HTTP状态码错误： Status Code: 403`)

	tr.RecordFailure("/posts", err403)
	tr.RecordFailure("/posts", err403)
	tr.RecordSuccess(false) // /profile success: must NOT reset the counter
	tr.RecordFailure("/posts", err403)
	if blocked, _, _ := tr.State(); !blocked {
		t.Fatal("profile success must not reset the failure counter")
	}
}

func TestDataPlaneSuccessUnblocks(t *testing.T) {
	tr := New(nil, nil)
	err403 := errors.New(`sidecar /posts: HTTP 502: HTTP状态码错误： Status Code: 403`)
	for i := 0; i < blockThreshold; i++ {
		tr.RecordFailure("/posts", err403)
	}
	tr.RecordSuccess(true)
	if blocked, reason, since := tr.State(); blocked || reason != "" || since != "" {
		t.Fatalf("expected cleared state, got %v %q %q", blocked, reason, since)
	}
}

func TestResetOnCookieChange(t *testing.T) {
	tr := New(nil, nil)
	err403 := errors.New(`sidecar /posts: HTTP 502: HTTP状态码错误： Status Code: 403`)
	for i := 0; i < blockThreshold; i++ {
		tr.RecordFailure("/posts", err403)
	}
	tr.Reset()
	if blocked, _, _ := tr.State(); blocked {
		t.Fatal("Reset must clear the block")
	}
}

// A blocked transition must publish exactly one provider.status event.
func TestPublishOnTransitions(t *testing.T) {
	bus := events.New()
	ch := bus.Subscribe(t.Context())
	tr := New(bus, func() string { return "running" })
	err403 := errors.New(`sidecar /posts: HTTP 502: HTTP状态码错误： Status Code: 403`)

	for i := 0; i < blockThreshold; i++ {
		tr.RecordFailure("/posts", err403)
	}
	evt := <-ch
	if evt.Type != events.TypeProviderStatus {
		t.Fatalf("event type = %q", evt.Type)
	}
	ps, ok := evt.Data.(events.ProviderStatus)
	if !ok || !ps.CookieBlocked || ps.BlockedReason == nil || ps.Sidecar != "running" {
		t.Fatalf("blocked event = %+v", evt.Data)
	}
	// Further failures: no more events (already blocked).
	tr.RecordFailure("/posts", err403)
	select {
	case extra := <-ch:
		t.Fatalf("unexpected extra event: %+v", extra.Data)
	default:
	}
	// Unblock publishes once with blocked=false.
	tr.Reset()
	evt = <-ch
	ps, ok = evt.Data.(events.ProviderStatus)
	if !ok || ps.CookieBlocked || ps.BlockedReason != nil {
		t.Fatalf("unblocked event = %+v", evt.Data)
	}
}
