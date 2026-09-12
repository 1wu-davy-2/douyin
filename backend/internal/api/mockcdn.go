package api

// Mock CDN: a deterministic fake media server for DY_MOCK=1 so the whole
// scan -> download -> SSE progress -> assets/playback chain runs offline.
//
//	Routes (public, no session - it plays the role of an external CDN):
//	  GET /mockcdn/{item_id}/{quality}.mp4 -> deterministic 2 MiB of
//	        pseudo-random bytes, streamed in 4 KiB chunks with a tiny sleep so
//	        download progress is observable.
//	  GET /mockcdn/{item_id}/cover.jpg     -> 1 KiB deterministic image body.
//
// The mock provider (internal/provider/mock.go) returns RELATIVE URLs of this
// shape; the downloader anchors them at its BaseURL (http://127.0.0.1:<port>).

import (
	"bytes"
	"hash/fnv"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// mock CDN payload geometry.
const (
	mockVideoBytes = 2 << 20 // 2 MiB per video variant
	mockCoverBytes = 1024    // 1 KiB cover
	mockChunkSize  = 4096    // 4 KiB write chunks
	mockChunkDelay = 4 * time.Millisecond
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
		writeMockPayload(w, "cover/"+item, mockCoverBytes, "image/jpeg", 0)
	case "540p.mp4", "720p.mp4", "1080p.mp4":
		writeMockPayload(w, item+"/"+name, mockVideoBytes, "video/mp4", mockChunkDelay)
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

// mockPayload produces size deterministic pseudo-random bytes seeded by the
// request key (same key -> same bytes, every run).
func mockPayload(key string, size int) []byte {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	rng := rand.New(rand.NewSource(int64(h.Sum32())))
	buf := make([]byte, size)
	_, _ = rng.Read(buf)
	return buf
}

// writeMockPayload emits the payload with Content-Length, optionally paced in
// 4 KiB chunks (making progress visible in the SSE stream).
func writeMockPayload(w http.ResponseWriter, key string, size int, contentType string, chunkDelay time.Duration) {
	payload := mockPayload(key, size)
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
