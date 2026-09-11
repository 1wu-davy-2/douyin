package api

// SSE event stream: GET /api/events (authenticated).
//
// Wire format per docs/api.md:
//
//	event: <type>\ndata: <json>\n\n
//
// with a ": ping" keep-alive every 15 seconds. Payloads are marshaled from the
// typed structs in internal/events so field names always match the contract.
// Client disconnects cancel the request context, which removes the bus
// subscription.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const ssePingInterval = 15 * time.Second

func (s *Server) registerEventRoutes(mux *http.ServeMux) {
	register(mux, []endpoint{
		{http.MethodGet, "/api/events", s.handleEvents},
	})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	// Subscribe before writing the response header so no event can be
	// published in the window between the client seeing "connected" and the
	// subscription being registered.
	sub := s.deps.Bus.Subscribe(r.Context())

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ping := time.NewTicker(ssePingInterval)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			// Client disconnected; the bus removes the subscription via ctx.
			return
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case evt := <-sub:
			data, err := json.Marshal(evt.Data)
			if err != nil {
				data = []byte("null")
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", evt.Type, data); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
