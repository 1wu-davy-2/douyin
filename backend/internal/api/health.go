package api

// Health endpoint (public): liveness plus effective provider/sidecar state.

import "net/http"

// handleHealth GET /api/health ->
// {status:"ok", provider:"mock"|"sidecar", sidecar:stopped|starting|running, real_scan_ready:bool}
//
// real_scan_ready reports whether real (non-mock) scanning is the effective
// mode — i.e. provider_mode resolves to the sidecar provider.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	isMock := s.deps.Resolver.IsMock(r.Context())
	providerName := "sidecar"
	if isMock {
		providerName = "mock"
	}
	writeJSON(w, http.StatusOK, struct {
		Status        string `json:"status"`
		Provider      string `json:"provider"`
		Sidecar       string `json:"sidecar"`
		RealScanReady bool   `json:"real_scan_ready"`
	}{
		Status:        "ok",
		Provider:      providerName,
		Sidecar:       s.deps.Manager.Status(),
		RealScanReady: !isMock,
	})
}

func (s *Server) registerHealthRoutes(mux *http.ServeMux) {
	register(mux, []endpoint{
		{http.MethodGet, "/api/health", s.handleHealth},
	})
}
