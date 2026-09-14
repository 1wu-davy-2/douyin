// Package events implements the typed in-process event bus feeding the SSE
// stream (GET /api/events).
//
// Delivery semantics (docs/api.md):
//   - every subscriber gets an independent buffered channel (capacity 256)
//   - progress-class events (download.progress / scan.progress) are
//     best-effort: when a subscriber's buffer is full the event is dropped
//     (slow SSE consumers must never stall download/scan workers)
//   - state-class events (download.status / scan.done / provider.status) MUST
//     be delivered: publishing blocks until the subscriber drains or its
//     context is canceled (client disconnected)
package events

import (
	"context"
	"log"
	"sync"
)

// Event type constants — identical to the SSE event names in docs/api.md.
const (
	TypeDownloadProgress = "download.progress"
	TypeDownloadStatus   = "download.status"
	TypeScanProgress     = "scan.progress"
	TypeScanDone         = "scan.done"
	TypeProviderStatus   = "provider.status"

	// Spark (续火花) integration events — docs/HUOHUA_EXECUTION_PLAN.md §6.6.
	TypeSparkAccountStatus  = "spark.account.status"
	TypeSparkSendProgress   = "spark.send.progress"
	TypeSparkSendFinished   = "spark.send.finished"
	TypeSparkFriendsUpdated = "spark.friends.updated"
	TypeSparkLoginStatus    = "spark.login.status"
)

// Event is a single bus message. Data carries one of the payload structs
// below (or any JSON-serializable value); the SSE handler marshals it with
// the contract's field names.
type Event struct {
	Type string
	Data any
}

// Payload structs for the contract's SSE events. Field names are frozen by
// docs/api.md — do not change the json tags.

// DownloadProgress is emitted while a job is downloading (throttled by the
// downloader to at most 1 per second per job).
type DownloadProgress struct {
	JobID           int64 `json:"job_id"`
	DownloadedBytes int64 `json:"downloaded_bytes"`
	TotalBytes      int64 `json:"total_bytes"`
	SpeedBps        int64 `json:"speed_bps"`
}

// DownloadStatus is emitted whenever a job changes state.
type DownloadStatus struct {
	JobID  int64   `json:"job_id"`
	Status string  `json:"status"`
	Error  *string `json:"error"`
	WorkID int64   `json:"work_id"`
}

// ScanProgress is emitted per scanned page.
type ScanProgress struct {
	ScanID       int64  `json:"scan_id"`
	CreatorID    int64  `json:"creator_id"`
	Page         int    `json:"page"`
	NewCount     int    `json:"new_count"`
	UpdatedCount int    `json:"updated_count"`
	Status       string `json:"status"`
}

// ScanDone is emitted when a scan run finishes.
type ScanDone struct {
	ScanID       int64   `json:"scan_id"`
	CreatorID    int64   `json:"creator_id"`
	Status       string  `json:"status"` // succeeded | partial | failed
	Pages        int     `json:"pages"`
	NewCount     int     `json:"new_count"`
	Completeness int     `json:"completeness"`
	LastError    *string `json:"last_error"`
}

// ProviderStatus is emitted on sidecar state changes / risk-control pauses.
type ProviderStatus struct {
	Sidecar     string  `json:"sidecar"` // stopped | starting | running
	RiskPaused  bool    `json:"risk_paused"`
	PausedUntil *string `json:"paused_until"`
}

// SparkAccountStatus is emitted whenever an account's run state changes.
type SparkAccountStatus struct {
	AccountID int64  `json:"account_id"`
	Status    string `json:"status"` // idle|sending|login_required|cooldown|error
	Detail    string `json:"detail"`
}

// SparkSendProgress is emitted per send result (target-level).
type SparkSendProgress struct {
	AccountID int64  `json:"account_id"`
	Target    string `json:"target"`
	State     string `json:"state"` // strong|failed
	Category  string `json:"category"`
	Detail    string `json:"detail"`
}

// SparkSendFinished is emitted once per account send run.
type SparkSendFinished struct {
	AccountID int64 `json:"account_id"`
	Strong    int   `json:"strong"`
	Weak      int   `json:"weak"`
	Failed    int   `json:"failed"`
}

// SparkFriendsUpdated is emitted after a friends refresh replaced the table.
type SparkFriendsUpdated struct {
	AccountID int64 `json:"account_id"`
	Count     int   `json:"count"`
}

// SparkLoginStatus is emitted on login flow state changes.
type SparkLoginStatus struct {
	LoggedIn bool   `json:"logged_in"`
	UniqueID string `json:"unique_id"`
}

// subscriberBuffer is the per-subscriber channel capacity (contract: 256).
const subscriberBuffer = 256

type subscriber struct {
	ctx context.Context
	ch  chan Event
}

// Bus fans events out to subscribers. The zero value is not usable; use New.
type Bus struct {
	mu   sync.RWMutex
	subs map[*subscriber]struct{}
}

// New creates an empty bus.
func New() *Bus {
	return &Bus{subs: make(map[*subscriber]struct{})}
}

// Subscribe registers a new subscriber with its own 256-slot buffer and
// returns the receive side. When ctx is canceled (e.g. the SSE client
// disconnects) the subscription is removed automatically.
func (b *Bus) Subscribe(ctx context.Context) <-chan Event {
	sub := &subscriber{ctx: ctx, ch: make(chan Event, subscriberBuffer)}
	b.mu.Lock()
	b.subs[sub] = struct{}{}
	b.mu.Unlock()
	go func() {
		<-ctx.Done()
		b.mu.Lock()
		delete(b.subs, sub)
		b.mu.Unlock()
		// The channel itself is never closed: a concurrent Publish may still
		// hold it in a pre-removal snapshot, and closing a channel with
		// in-flight senders panics. Garbage collection reclaims it.
	}()
	return sub.ch
}

// mustDeliver reports whether the event type is state-class (must reach every
// subscriber). Unknown types default to must-deliver — losing a future state
// event is worse than briefly blocking its publisher.
func mustDeliver(eventType string) bool {
	switch eventType {
	case TypeDownloadProgress, TypeScanProgress, TypeSparkSendProgress:
		return false
	default:
		return true
	}
}

// Publish fans evt out to all current subscribers. Never blocks for
// progress-class events (drops on full buffer); blocks for state-class events
// until delivered or the subscriber's context is canceled (logged).
func (b *Bus) Publish(evt Event) {
	b.mu.RLock()
	subs := make([]*subscriber, 0, len(b.subs))
	for sub := range b.subs {
		subs = append(subs, sub)
	}
	b.mu.RUnlock()

	for _, sub := range subs {
		select {
		case sub.ch <- evt:
			continue
		default:
		}
		if !mustDeliver(evt.Type) {
			// Progress-class: drop rather than stall the producer.
			continue
		}
		log.Printf("[events] subscriber buffer full, blocking to deliver %q", evt.Type)
		select {
		case sub.ch <- evt:
		case <-sub.ctx.Done():
			log.Printf("[events] dropped %q for disconnected subscriber", evt.Type)
		}
	}
}
