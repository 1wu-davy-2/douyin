package provider

import (
	"context"
	"strings"

	"douyin/backend/internal/config"
	"douyin/backend/internal/risk"
	"douyin/backend/internal/sidecar"
)

// Build returns the provider selected purely from environment configuration:
// DY_MOCK=1 -> MockProvider, otherwise SidecarProvider (wrapping the Manager).
func Build(cfg config.Settings, mgr *sidecar.Manager) Provider {
	if cfg.Mock {
		return NewMockProvider()
	}
	return NewSidecarProvider(mgr)
}

// ModeSource supplies the runtime provider_mode setting
// ("auto" | "sidecar" | "mock") so that a settings change takes effect on the
// next request without a process restart. settings.Store implements it.
type ModeSource interface {
	ProviderMode(ctx context.Context) string
}

// Resolver picks the effective provider per call:
//   - mode "mock"   -> MockProvider
//   - mode "sidecar"-> SidecarProvider
//   - mode "auto"   -> DY_MOCK decides (env fallback)
type Resolver struct {
	cfg  config.Settings
	mgr  *sidecar.Manager
	mode ModeSource
	risk *risk.Tracker
}

// NewResolver wires the resolver together.
func NewResolver(cfg config.Settings, mgr *sidecar.Manager, mode ModeSource) *Resolver {
	return &Resolver{cfg: cfg, mgr: mgr, mode: mode}
}

// SetRiskTracker attaches the shared risk tracker; every SidecarProvider the
// resolver hands out gets a reference.
func (r *Resolver) SetRiskTracker(t *risk.Tracker) {
	r.risk = t
}

// IsMock reports whether the effective provider is the offline mock.
func (r *Resolver) IsMock(ctx context.Context) bool {
	switch strings.TrimSpace(r.mode.ProviderMode(ctx)) {
	case "mock":
		return true
	case "sidecar":
		return false
	default: // auto
		return r.cfg.Mock
	}
}

// Resolve builds the provider for the current call. SidecarProvider instances
// are stateless (all state lives in the Manager), so constructing one per
// request is cheap and keeps provider_mode/cookie changes hot.
func (r *Resolver) Resolve(ctx context.Context) Provider {
	if r.IsMock(ctx) {
		return NewMockProvider()
	}
	p := NewSidecarProvider(r.mgr)
	p.SetRiskTracker(r.risk)
	return p
}
