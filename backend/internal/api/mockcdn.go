package api

// Mock CDN: a fake media server for DY_MOCK=1 so the whole
// scan -> download -> SSE progress -> assets/playback chain runs offline.
//
//	Routes (public, no session - it plays the role of an external CDN):
//	  GET /mockcdn/{item_id}/{quality}.mp4 -> a real, tiny CC-BY sample MP4
//	        (per-tier byte prefixes, see internal/mockmedia), streamed in
//	        chunks with a small sleep so download progress is observable.
//	        Real container bytes keep the built-in player honest:
//	        pseudo-random payloads fail Chrome's demuxer with
//	        DEMUXER_ERROR_COULD_NOT_OPEN.
//	  GET /mockcdn/{item_id}/cover.jpg     -> generated 128x128 JPEG.
//
// The mock provider (internal/provider/mock.go) returns RELATIVE URLs of this
// shape; the downloader anchors them at its BaseURL (http://127.0.0.1:<port>).

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"douyin/backend/internal/mockmedia"
)

// Chunk pacing keeps SSE progress visible (~2.6 MiB/s).
const (
	mockChunkSize  = 64 << 10
	mockChunkDelay = 25 * time.Millisecond
)

// registerMockCDN mounts the fake CDN routes; only called in mock mode.
func (s *Server) registerMockCDN(mux *http.ServeMux) {
	mux.HandleFunc("GET /mockcdn/", s.serveMockCDN)
}

// serveMockCDN answers /mockcdn/{item}/{name}.
func (s *Server) serveMockCDN(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/mockcdn/")
	item, name, ok := strings.Cut(rest, "/")
	if !ok || item == "" || name == "" || strings.Contains(name, "/") {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	switch name {
	case "cover.jpg":
		writeMockPayload(w, mockmedia.CoverJPEG(), "image/jpeg", 0)
	case "1080p.mp4":
		writeMockPayload(w, mockmedia.SampleVideo, "video/mp4", mockChunkDelay)
	case "720p.mp4":
		writeMockPayload(w, mockmedia.Sample720Video, "video/mp4", mockChunkDelay)
	case "540p.mp4":
		writeMockPayload(w, mockmedia.Sample540Video, "video/mp4", mockChunkDelay)
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

// writeMockPayload emits the payload with Content-Length, optionally paced in
// chunks (making progress visible in the SSE stream).
func writeMockPayload(w http.ResponseWriter, payload []byte, contentType string, chunkDelay time.Duration) {
	size := len(payload)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(size))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)

	if chunkDelay <= 0 {
		_, _ = io.Copy(w, bytes.NewReader(payload))
		return
	}
	for written := 0; written < size; written += mockChunkSize {
		end := written + mockChunkSize
		if end > size {
			end = size
		}
		if _, err := w.Write(payload[written:end]); err != nil {
			return // client gone (canceled download)
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(chunkDelay)
	}
}
