// Package config loads process configuration from environment variables.
//
// Environment variables (all optional):
//
//	DY_PORT                 HTTP listen port (default 8787)
//	DY_DATA_DIR             data directory (default ./data)
//	DY_MOCK                 "1" -> use the built-in Go mock provider
//	DY_SIDECAR_PORT         sidecar listen port (default 18787)
//	DY_SIDECAR_PYTHON       python executable for the sidecar;
//	                        empty -> sidecar/.venv/Scripts/python.exe if it
//	                        exists (relative to cwd), else "python"
//	DY_SIDECAR_IDLE_TIMEOUT idle timeout for the sidecar process,
//	                        e.g. "10m" or plain minutes "10" (default 10m)
//	DY_DB_PATH              SQLite file path override (default <data_dir>/app.db)
package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// venvPython is the sidecar venv interpreter layout created for this project
// (see sidecar/requirements.txt / sidecar/.venv).
const venvPython = "sidecar/.venv/Scripts/python.exe"

// Settings is the resolved process configuration.
type Settings struct {
	Port    int    // DY_PORT
	DataDir string // DY_DATA_DIR
	Mock    bool   // DY_MOCK

	SidecarPort        int           // DY_SIDECAR_PORT
	SidecarPython      string        // DY_SIDECAR_PYTHON (resolved)
	SidecarIdleTimeout time.Duration // DY_SIDECAR_IDLE_TIMEOUT

	DBPath string // DY_DB_PATH or <DataDir>/app.db
}

// Load reads the environment and resolves all settings.
func Load() Settings {
	s := Settings{
		Port:               envInt("DY_PORT", 8787),
		DataDir:            envNonEmpty("DY_DATA_DIR", "./data"),
		Mock:               envBool("DY_MOCK"),
		SidecarPort:        envInt("DY_SIDECAR_PORT", 18787),
		SidecarPython:      strings.TrimSpace(os.Getenv("DY_SIDECAR_PYTHON")),
		SidecarIdleTimeout: envDurationMinutes("DY_SIDECAR_IDLE_TIMEOUT", 10*time.Minute),
	}

	if v := strings.TrimSpace(os.Getenv("DY_DB_PATH")); v != "" {
		s.DBPath = v
	} else {
		s.DBPath = filepath.Join(s.DataDir, "app.db")
	}

	if s.SidecarPython == "" {
		// Auto-detect the project venv interpreter (cwd is the repo root per
		// project convention); fall back to whatever "python" resolves to.
		if _, err := os.Stat(venvPython); err == nil {
			s.SidecarPython = venvPython
		} else {
			s.SidecarPython = "python"
		}
	}
	return s
}

func envNonEmpty(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// envDurationMinutes accepts Go durations ("10m", "90s") and plain integers
// that are interpreted as minutes.
func envDurationMinutes(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return time.Duration(n) * time.Minute
	}
	return def
}
