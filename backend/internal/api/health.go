package api

// Health endpoint (public): liveness plus effective provider/sidecar state.

import "net/http"

// handleHealth GET /api/health ->
// {status:"ok", provider:"mock"|"sidecar", sidecar:stopped|starting|running,
//  real_scan_ready:bool, cookie_blocked:bool, blocked_reason, blocked_since}
//
// real_scan_ready reports whether real (non-mock) scanning is the effective
// mode — i.e. provider_mode resolves to the sidecar provider.
// cookie_blocked (contract v1.4, appended): douyin risk control / dead cookie
// detected by the risk tracker; the UI shows a global refresh-cookie prompt.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	isMock := s.deps.Resolver.IsMock(r.Context())
	providerName := "sidecar"
	if isMock {
		providerName = "mock"
	}
	var blocked bool
	var reason, since string
	if s.deps.Risk != nil {
		blocked, reason, since = s.deps.Risk.State()
	}
	var reasonPtr, sincePtr *string
	if blocked && reason != "" {
		reasonPtr = &reason
	}
	if blocked && since != "" {
		sincePtr = &since
	}
	writeJSON(w, http.StatusOK, struct {
		Status        string  `json:"status"`
		Provider      string  `json:"provider"`
		Sidecar       string  `json:"sidecar"`
		RealScanReady bool    `json:"real_scan_ready"`
		CookieBlocked bool    `json:"cookie_blocked"`
		BlockedReason *string `json:"blocked_reason"`
		BlockedSince  *string `json:"blocked_since"`
	}{
		Status:        "ok",
		Provider:      providerName,
		Sidecar:       s.deps.Manager.Status(),
		RealScanReady: !isMock,
		CookieBlocked: blocked,
		BlockedReason: reasonPtr,
		BlockedSince:  sincePtr,
	})
}

func (s *Server) registerHealthRoutes(mux *http.ServeMux) {
	register(mux, []endpoint{
		{http.MethodGet, "/api/health", s.handleHealth},
	})
}
