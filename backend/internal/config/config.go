// Package config loads process configuration from environment variables.
//
// Environment variables (all optional):
//
//	DY_PORT                 HTTP listen port (default 8787)
//	DY_BIND                 HTTP listen address (default "127.0.0.1";
//	                        Docker 部署设为 "0.0.0.0" 才能从容器外访问)
//	DY_DATA_DIR             data directory (default ./data)
//	DY_DOWNLOAD_ROOT        default download root (runtime setting may override)
//	DY_DOWNLOAD_LIMIT_GB    total downloaded-size cap in GB; 0 = unlimited (default 20)
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
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Settings is the resolved process configuration.
type Settings struct {
	Port         int    // DY_PORT
	Bind         string // DY_BIND (listen host; "127.0.0.1" default)
	DataDir      string // DY_DATA_DIR
	DownloadRoot string // DY_DOWNLOAD_ROOT (empty -> <data_dir>/downloads)
	DownloadLimitGB int // DY_DOWNLOAD_LIMIT_GB (0 = unlimited)
	Mock         bool   // DY_MOCK

	SidecarPort        int           // DY_SIDECAR_PORT
	SidecarPython      string        // DY_SIDECAR_PYTHON (resolved)
	SidecarIdleTimeout time.Duration // DY_SIDECAR_IDLE_TIMEOUT

	DBPath string // DY_DB_PATH or <DataDir>/app.db
}

// Load reads the environment and resolves all settings.
func Load() Settings {
	s := Settings{
		Port:               envInt("DY_PORT", 8787),
		Bind:               envNonEmpty("DY_BIND", "127.0.0.1"),
		DataDir:            envNonEmpty("DY_DATA_DIR", "./data"),
		DownloadRoot:       envNonEmpty("DY_DOWNLOAD_ROOT", ""),
		DownloadLimitGB:    envInt("DY_DOWNLOAD_LIMIT_GB", 20),
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
		// project convention); fall back to whatever "python3"/"python"
		// resolves to. Windows venvs use Scripts/python.exe, POSIX use
		// bin/python.
		for _, candidate := range venvPythonCandidates() {
			if _, err := os.Stat(candidate); err == nil {
				s.SidecarPython = candidate
				break
			}
		}
		if s.SidecarPython == "" {
			s.SidecarPython = defaultPython()
		}
	}
	return s
}

// venvPythonCandidates lists the venv interpreter layouts per platform, most
// specific first (Windows "Scripts/python.exe", POSIX "bin/python3"/"bin/python").
func venvPythonCandidates() []string {
	if runtime.GOOS == "windows" {
		return []string{filepath.Join("sidecar", ".venv", "Scripts", "python.exe")}
	}
	return []string{
		filepath.Join("sidecar", ".venv", "bin", "python3"),
		filepath.Join("sidecar", ".venv", "bin", "python"),
	}
}

func defaultPython() string {
	if runtime.GOOS != "windows" {
		// Prefer python3 on POSIX distros where bare "python" may not exist.
		if _, err := exec.LookPath("python3"); err == nil {
			return "python3"
		}
	}
	return "python"
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
