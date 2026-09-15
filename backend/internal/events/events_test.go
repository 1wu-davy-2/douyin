package events

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func progressEvents(n int) []Event {
	evts := make([]Event, n)
	for i := range evts {
		evts[i] = Event{Type: TypeDownloadProgress, Data: DownloadProgress{JobID: int64(i)}}
	}
	return evts
}

func publishAll(b *Bus, evts []Event) {
	for _, evt := range evts {
		b.Publish(evt)
	}
}

func recv(t *testing.T, ch <-chan Event, timeout time.Duration) (Event, bool) {
	t.Helper()
	select {
	case evt := <-ch:
		return evt, true
	case <-time.After(timeout):
		return Event{}, false
	}
}

// Progress events are best-effort: when the 256-slot buffer of a subscriber
// that never reads is overfilled, the excess is dropped, not blocked.
func TestBusProgressEventsDroppedWhenFull(t *testing.T) {
	bus := New()
	ch := bus.Subscribe(context.Background())

	publishAll(bus, progressEvents(300)) // 44 more than the buffer holds

	// Exactly the buffered 256 must be receivable, then the channel is empty.
	for i := 0; i < 256; i++ {
		if _, ok := recv(t, ch, time.Second); !ok {
			t.Fatalf("event %d not received (buffer drained early)", i)
		}
	}
	select {
	case evt := <-ch:
		t.Fatalf("unexpected extra event: %+v", evt)
	default:
	}
}

// State events must be delivered even when the buffer is full: the publisher
// blocks until the subscriber drains.
func TestBusStatusEventsMustDeliver(t *testing.T) {
	bus := New()
	ch := bus.Subscribe(context.Background())

	publishAll(bus, progressEvents(256)) // fill the buffer exactly

	done := make(chan struct{})
	go func() {
		defer close(done)
		bus.Publish(Event{Type: TypeDownloadStatus, Data: DownloadStatus{JobID: 99, Status: "succeeded"}})
	}()

	for i := 0; i < 256; i++ {
		evt, ok := recv(t, ch, time.Second)
		if !ok || evt.Type != TypeDownloadProgress {
			t.Fatalf("event %d: expected buffered progress event", i)
		}
	}
	evt, ok := recv(t, ch, 2*time.Second)
	if !ok {
		t.Fatal("status event not delivered — Publish dropped a must-deliver event")
	}
	if evt.Type != TypeDownloadStatus {
		t.Fatalf("got %q, want download.status", evt.Type)
	}
	select {
	case <-done:
	default:
		t.Fatal("Publish did not return after delivery")
	}
}

// A disconnected subscriber (canceled context) must unblock a stuck Publish.
func TestBusPublishUnblockedByDisconnect(t *testing.T) {
	bus := New()
	ctx, cancel := context.WithCancel(context.Background())
	bus.Subscribe(ctx)

	publishAll(bus, progressEvents(256))

	done := make(chan struct{})
	go func() {
		defer close(done)
		bus.Publish(Event{Type: TypeScanDone, Data: ScanDone{ScanID: 5}})
	}()

	// Give Publish time to block on the full buffer, then disconnect.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish still blocked after subscriber disconnected")
	}
	// The abandoned channel stays unusable but nothing panics; bus keeps
	// working for other subscribers.
	ch2 := bus.Subscribe(context.Background())
	bus.Publish(Event{Type: TypeScanDone, Data: ScanDone{ScanID: 6}})
	if evt, ok := recv(t, ch2, time.Second); !ok || evt.Data.(ScanDone).ScanID != 6 {
		t.Fatal("bus broken after unsubscribe")
	}
}

// Broadcast reaches multiple independent subscribers, each with its own buffer.
func TestBusMultipleSubscribers(t *testing.T) {
	bus := New()
	ch1 := bus.Subscribe(context.Background())
	ch2 := bus.Subscribe(context.Background())

	bus.Publish(Event{Type: TypeProviderStatus, Data: ProviderStatus{Sidecar: "running"}})

	for name, ch := range map[string]<-chan Event{"s1": ch1, "s2": ch2} {
		evt, ok := recv(t, ch, time.Second)
		if !ok {
			t.Fatalf("%s: event not received", name)
		}
		if evt.Data.(ProviderStatus).Sidecar != "running" {
			t.Fatalf("%s: wrong payload %+v", name, evt.Data)
		}
	}
}

// SSE payload JSON field names must match the frozen contract exactly.
func TestPayloadJSONFieldNames(t *testing.T) {
	cases := []struct {
		name string
		data any
		want string
	}{
		{"download.progress", DownloadProgress{JobID: 99, DownloadedBytes: 1, TotalBytes: 2, SpeedBps: 3},
			`{"job_id":99,"downloaded_bytes":1,"total_bytes":2,"speed_bps":3}`},
		{"download.status", DownloadStatus{JobID: 99, Status: "succeeded", WorkID: 123},
			`{"job_id":99,"status":"succeeded","error":null,"work_id":123}`},
		{"scan.progress", ScanProgress{ScanID: 5, CreatorID: 1, Page: 12, NewCount: 180, UpdatedCount: 2, Status: "running"},
			`{"scan_id":5,"creator_id":1,"page":12,"new_count":180,"updated_count":2,"status":"running"}`},
		{"scan.done", ScanDone{ScanID: 5, CreatorID: 1, Status: "succeeded", Pages: 26, NewCount: 502, Completeness: 0},
			`{"scan_id":5,"creator_id":1,"status":"succeeded","pages":26,"new_count":502,"completeness":0,"last_error":null}`},
		{"provider.status", ProviderStatus{Sidecar: "running"},
			`{"sidecar":"running","risk_paused":false,"paused_until":null,"cookie_blocked":false,"blocked_reason":null}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.data)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(raw) != tc.want {
				t.Fatalf("json = %s, want %s", raw, tc.want)
			}
		})
	}
}
