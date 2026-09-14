package spark

import (
	"encoding/json"
	"strings"
)

// SendConfig mirrors spark_settings.send_config
// (docs/HUOHUA_EXECUTION_PLAN.md §5.4). It is stored as JSON in a single
// spark_settings row; missing/broken values fall back to DefaultSendConfig.
//
// Rate-limit floors (messageIntervalSecondsMin >= 25) are a hard rule
// (§10.3): platform risk control — do not lower the clamps below.
type SendConfig struct {
	MessageTemplate string   `json:"messageTemplate"`
	MessageVariants []string `json:"messageVariants"`
	HitokotoTypes   []string `json:"hitokotoTypes"`

	SendWindow          SendWindow   `json:"sendWindow"`
	SendStrategy        SendStrategy `json:"sendStrategy"`
	FriendScan          FriendScan   `json:"friendScan"`
	AccountFailurePause FailurePause `json:"accountFailurePause"`
}

// SendWindow bounds the scheduled sending window (Asia/Shanghai local hours).
// IntervalMinutes doubles as the grace period past EndHour (§6.4).
type SendWindow struct {
	Enabled         bool `json:"enabled"`
	StartHour       int  `json:"startHour"`
	EndHour         int  `json:"endHour"`
	IntervalMinutes int  `json:"intervalMinutes"`
}

// SendStrategy carries both the Go-side pacing (account start delays, target
// shuffle) and the engine-side pacing (message intervals, message variants —
// upstream reads variants from sendStrategy.messageVariants).
type SendStrategy struct {
	ShuffleTargets              bool `json:"shuffleTargets"`
	AccountStartDelaySecondsMin int  `json:"accountStartDelaySecondsMin"`
	AccountStartDelaySecondsMax int  `json:"accountStartDelaySecondsMax"`
	MessageIntervalSecondsMin   int  `json:"messageIntervalSecondsMin"`
	MessageIntervalSecondsMax   int  `json:"messageIntervalSecondsMax"`
}

// FriendScan configures the engine's friend-list scrolling (upstream
// friendListScan key).
type FriendScan struct {
	MaxScanSeconds     int     `json:"maxScanSeconds"`
	IdleScanSeconds    int     `json:"idleScanSeconds"`
	ScrollStepPx       int     `json:"scrollStepPx"`
	ScrollDelaySeconds float64 `json:"scrollDelaySeconds"`
}

// FailurePause is the account-level cooldown policy: after `Attempts`
// account-category failures within one local day the account cools down for
// CooldownMinutes.
type FailurePause struct {
	Attempts        int `json:"attempts"`
	CooldownMinutes int `json:"cooldownMinutes"`
}

// DefaultSendConfig returns the §5.4 defaults (also seeded by migration 0006).
func DefaultSendConfig() SendConfig {
	return SendConfig{
		MessageTemplate: "✨今日火花+1",
		MessageVariants: []string{
			"🤩今日火花+1",
			"今天来补个火花",
			"给你续一下今天的火花",
			"路过给你加个小火花",
		},
		HitokotoTypes: []string{"文学", "影视", "诗词", "哲学"},
		SendWindow: SendWindow{
			Enabled:         true,
			StartHour:       10,
			EndHour:         18,
			IntervalMinutes: 20,
		},
		SendStrategy: SendStrategy{
			ShuffleTargets:              true,
			AccountStartDelaySecondsMin: 15,
			AccountStartDelaySecondsMax: 60,
			MessageIntervalSecondsMin:   25,
			MessageIntervalSecondsMax:   70,
		},
		FriendScan: FriendScan{
			MaxScanSeconds:     300,
			IdleScanSeconds:    120,
			ScrollStepPx:       400,
			ScrollDelaySeconds: 0.8,
		},
		AccountFailurePause: FailurePause{
			Attempts:        3,
			CooldownMinutes: 60,
		},
	}
}

// ParseSendConfig decodes the stored JSON, falling back to the defaults when
// the value is empty or corrupt. Individually missing fields keep their
// zero values — the UI always PUTs the full object, so this is best-effort.
func ParseSendConfig(raw string) SendConfig {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultSendConfig()
	}
	var cfg SendConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return DefaultSendConfig()
	}
	return cfg.Normalize()
}

// Normalize clamps hostile values to safe bounds and applies defaults for
// unset groups. It never turns a config into a faster sender than the
// defaults (§10.3).
func (c SendConfig) Normalize() SendConfig {
	def := DefaultSendConfig()
	if c.MessageTemplate == "" && len(c.MessageVariants) == 0 {
		c.MessageTemplate = def.MessageTemplate
	}
	if len(c.MessageVariants) == 0 && c.MessageTemplate == "" {
		c.MessageVariants = def.MessageVariants
	}

	if c.SendWindow.StartHour < 0 || c.SendWindow.StartHour > 23 {
		c.SendWindow.StartHour = def.SendWindow.StartHour
	}
	if c.SendWindow.EndHour < 0 || c.SendWindow.EndHour > 23 {
		c.SendWindow.EndHour = def.SendWindow.EndHour
	}
	if c.SendWindow.EndHour <= c.SendWindow.StartHour {
		c.SendWindow.EndHour = def.SendWindow.EndHour
		c.SendWindow.StartHour = def.SendWindow.StartHour
	}
	if c.SendWindow.IntervalMinutes <= 0 {
		c.SendWindow.IntervalMinutes = def.SendWindow.IntervalMinutes
	}

	if c.SendStrategy.MessageIntervalSecondsMin < 25 {
		c.SendStrategy.MessageIntervalSecondsMin = 25
	}
	if c.SendStrategy.MessageIntervalSecondsMax < c.SendStrategy.MessageIntervalSecondsMin {
		c.SendStrategy.MessageIntervalSecondsMax = c.SendStrategy.MessageIntervalSecondsMin
	}
	if c.SendStrategy.MessageIntervalSecondsMax > 600 {
		c.SendStrategy.MessageIntervalSecondsMax = 600
	}
	if c.SendStrategy.AccountStartDelaySecondsMin < 0 {
		c.SendStrategy.AccountStartDelaySecondsMin = def.SendStrategy.AccountStartDelaySecondsMin
	}
	if c.SendStrategy.AccountStartDelaySecondsMax < c.SendStrategy.AccountStartDelaySecondsMin {
		c.SendStrategy.AccountStartDelaySecondsMax = c.SendStrategy.AccountStartDelaySecondsMin
	}
	if c.SendStrategy.AccountStartDelaySecondsMax > 600 {
		c.SendStrategy.AccountStartDelaySecondsMax = 600
	}

	if c.FriendScan.MaxScanSeconds <= 0 {
		c.FriendScan.MaxScanSeconds = def.FriendScan.MaxScanSeconds
	}
	if c.FriendScan.IdleScanSeconds <= 0 {
		c.FriendScan.IdleScanSeconds = def.FriendScan.IdleScanSeconds
	}
	if c.FriendScan.ScrollStepPx <= 0 {
		c.FriendScan.ScrollStepPx = def.FriendScan.ScrollStepPx
	}
	if c.FriendScan.ScrollDelaySeconds <= 0 {
		c.FriendScan.ScrollDelaySeconds = def.FriendScan.ScrollDelaySeconds
	}

	if c.AccountFailurePause.Attempts <= 0 {
		c.AccountFailurePause.Attempts = def.AccountFailurePause.Attempts
	}
	if c.AccountFailurePause.CooldownMinutes <= 0 {
		c.AccountFailurePause.CooldownMinutes = def.AccountFailurePause.CooldownMinutes
	}
	return c
}

// EnginePayload maps the stored config onto the keys the engine expects
// (upstream core/send_tools.py): messageTemplate/hitokotoTypes at the top
// level, variants inside sendStrategy, friend scan under "friendListScan".
func (c SendConfig) EnginePayload() map[string]any {
	return map[string]any{
		"messageTemplate": c.MessageTemplate,
		"hitokotoTypes":   c.HitokotoTypes,
		"sendStrategy": map[string]any{
			"messageVariants":             c.MessageVariants,
			"accountStartDelaySecondsMin": c.SendStrategy.AccountStartDelaySecondsMin,
			"accountStartDelaySecondsMax": c.SendStrategy.AccountStartDelaySecondsMax,
			"messageIntervalSecondsMin":   c.SendStrategy.MessageIntervalSecondsMin,
			"messageIntervalSecondsMax":   c.SendStrategy.MessageIntervalSecondsMax,
		},
		"friendListScan": map[string]any{
			"maxScanSeconds":     c.FriendScan.MaxScanSeconds,
			"idleScanSeconds":    c.FriendScan.IdleScanSeconds,
			"scrollStepPx":       c.FriendScan.ScrollStepPx,
			"scrollDelaySeconds": c.FriendScan.ScrollDelaySeconds,
		},
	}
}
