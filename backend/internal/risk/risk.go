// Package risk tracks douyin risk-control blocking observed by the provider
// layer (HTTP 403 / missing cookie) and broadcasts state transitions so the
// UI can prompt the user to refresh the cookie.
//
// Model: a small consecutive-failure counter. Any provider failure classified
// as risk-control increments it; reaching the threshold flips the tracker to
// blocked. A data-plane success (/posts, /work) or an explicit cookie change
// clears it. /profile success deliberately does NOT reset the counter — the
// profile endpoint passes even for half-dead cookies while the post list is
// already 403ing (observed 2026-09-15), so counting it would mask the block.
package risk

import (
	"log"
	"strings"
	"sync"
	"time"

	"douyin/backend/internal/events"
)

const (
	// ReasonRisk403: douyin rejected the request at the HTTP layer (F2 raises
	// "HTTP状态码错误： Status Code: 403" for the post list / detail APIs).
	ReasonRisk403 = "抖音风控拦截（HTTP 403）：Cookie 失效或当前 IP 请求频率过高"
	// ReasonEmptyShell: douyin answered 200 with an empty payload shell —
	// the classic risk-control/expired-cookie pattern the sidecar surfaces.
	ReasonEmptyShell = "抖音返回空数据（疑似风控或 Cookie 失效）"
	// ReasonNoCookie: the sidecar found no cookie file at all.
	ReasonNoCookie = "未配置 Cookie，请在设置页粘贴抖音 Cookie"

	// blockThreshold consecutive risk failures before blocking. One scan page
	// retry sequence produces up to 4 failures per cursor, so a dead cookie
	// trips this within the first page while a single transient blip does not.
	blockThreshold = 3
)

// Classify decides whether a provider error text means douyin risk control /
// cookie trouble, and returns the user-facing reason. Intra-process auth
// failures (sidecar token mismatch) and infrastructure errors are NOT risk.
func Classify(errText string) (reason string, isRisk bool) {
	switch {
	case strings.Contains(errText, "cookie not configured"):
		return ReasonNoCookie, true
	case strings.Contains(errText, "sidecar token"):
		// HTTP 403 from the sidecar's own token check — never douyin.
		return "", false
	case strings.Contains(errText, "Status Code: 403"):
		return ReasonRisk403, true
	case strings.Contains(errText, "risk control or expired cookie"):
		return ReasonEmptyShell, true
	}
	return "", false
}

// Tracker holds the current block state. Safe for concurrent use.
type Tracker struct {
	bus       *events.Bus
	sidecarFn func() string // current sidecar lifecycle state, for the event

	mu          sync.Mutex
	blocked     bool
	reason      string
	since       time.Time
	consecutive int
}

// New creates a tracker that publishes provider.status events on transitions.
// sidecarFn supplies the current sidecar state string for those events.
func New(bus *events.Bus, sidecarFn func() string) *Tracker {
	return &Tracker{bus: bus, sidecarFn: sidecarFn}
}

// RecordFailure feeds one provider call failure. endpoint is the sidecar path
// ("/profile" | "/posts" | "/work") and only informational for logging.
func (t *Tracker) RecordFailure(endpoint string, err error) {
	reason, isRisk := Classify(err.Error())
	if !isRisk {
		return
	}

	t.mu.Lock()
	t.consecutive++
	consec := t.consecutive
	flip := !t.blocked && consec >= blockThreshold
	if flip {
		t.blocked = true
		t.reason = reason
		t.since = time.Now().UTC()
	}
	t.mu.Unlock()

	if flip {
		log.Printf("[risk] blocked after %d consecutive risk failures (last: %s %v)", consec, endpoint, err)
		t.publish()
	}
}

// RecordSuccess feeds one provider call success. dataPlane=true only for
// /posts and /work — /profile success is ignored on purpose (see package doc).
func (t *Tracker) RecordSuccess(dataPlane bool) {
	if !dataPlane {
		return
	}
	t.mu.Lock()
	t.consecutive = 0
	wasBlocked := t.blocked
	t.blocked = false
	t.reason = ""
	t.since = time.Time{}
	t.mu.Unlock()

	if wasBlocked {
		log.Printf("[risk] unblocked: data-plane call succeeded")
		t.publish()
	}
}

// Reset clears the state unconditionally — call it when the cookie changes.
func (t *Tracker) Reset() {
	t.mu.Lock()
	t.consecutive = 0
	wasBlocked := t.blocked
	t.blocked = false
	t.reason = ""
	t.since = time.Time{}
	t.mu.Unlock()

	if wasBlocked {
		log.Printf("[risk] unblocked: cookie updated")
		t.publish()
	}
}

// State returns the current block snapshot; since is RFC3339 or empty.
func (t *Tracker) State() (blocked bool, reason string, since string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.blocked && !t.since.IsZero() {
		return true, t.reason, t.since.Format(time.RFC3339)
	}
	return t.blocked, t.reason, ""
}

// publish broadcasts a provider.status event carrying the block fields.
// provider.status is state-class: delivery is guaranteed by the bus.
func (t *Tracker) publish() {
	if t.bus == nil {
		return
	}
	blocked, reason, _ := t.State()
	var reasonPtr *string
	if blocked && reason != "" {
		reasonPtr = &reason
	}
	sidecarState := ""
	if t.sidecarFn != nil {
		sidecarState = t.sidecarFn()
	}
	t.bus.Publish(events.Event{
		Type: events.TypeProviderStatus,
		Data: events.ProviderStatus{
			Sidecar:       sidecarState,
			CookieBlocked: blocked,
			BlockedReason: reasonPtr,
		},
	})
}
