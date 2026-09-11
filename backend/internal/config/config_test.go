package config

import (
	"path/filepath"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	// Test cwd is the package dir: the venv python does not exist relative to
	// it, so the fallback "python" applies.
	t.Setenv("DY_PORT", "")
	t.Setenv("DY_DATA_DIR", "")
	t.Setenv("DY_MOCK", "")
	t.Setenv("DY_SIDECAR_PORT", "")
	t.Setenv("DY_SIDECAR_PYTHON", "")
	t.Setenv("DY_SIDECAR_IDLE_TIMEOUT", "")
	t.Setenv("DY_DB_PATH", "")

	s := Load()
	if s.Port != 8787 {
		t.Errorf("Port = %d, want 8787", s.Port)
	}
	if s.DataDir != "./data" {
		t.Errorf("DataDir = %q, want ./data", s.DataDir)
	}
	if s.Mock {
		t.Error("Mock must default to false")
	}
	if s.SidecarPort != 18787 {
		t.Errorf("SidecarPort = %d, want 18787", s.SidecarPort)
	}
	if s.SidecarPython != "python" {
		t.Errorf("SidecarPython = %q, want python", s.SidecarPython)
	}
	if s.SidecarIdleTimeout != 10*time.Minute {
		t.Errorf("SidecarIdleTimeout = %v, want 10m", s.SidecarIdleTimeout)
	}
	if s.DBPath != filepath.Join("./data", "app.db") {
		t.Errorf("DBPath = %q", s.DBPath)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("DY_PORT", "9000")
	t.Setenv("DY_DATA_DIR", "D:/tmp/dydata")
	t.Setenv("DY_MOCK", "1")
	t.Setenv("DY_SIDECAR_PORT", "18888")
	t.Setenv("DY_SIDECAR_PYTHON", "C:/Python312/python.exe")
	t.Setenv("DY_SIDECAR_IDLE_TIMEOUT", "5")
	t.Setenv("DY_DB_PATH", "D:/tmp/other.db")

	s := Load()
	if s.Port != 9000 || s.DataDir != "D:/tmp/dydata" || !s.Mock {
		t.Fatalf("basic overrides wrong: %+v", s)
	}
	if s.SidecarPort != 18888 || s.SidecarPython != "C:/Python312/python.exe" {
		t.Fatalf("sidecar overrides wrong: %+v", s)
	}
	if s.SidecarIdleTimeout != 5*time.Minute {
		t.Errorf("plain integer DY_SIDECAR_IDLE_TIMEOUT must mean minutes, got %v", s.SidecarIdleTimeout)
	}
	if s.DBPath != "D:/tmp/other.db" {
		t.Errorf("DBPath override ignored: %q", s.DBPath)
	}
}

func TestLoadIdleTimeoutDuration(t *testing.T) {
	t.Setenv("DY_SIDECAR_IDLE_TIMEOUT", "90s")
	if s := Load(); s.SidecarIdleTimeout != 90*time.Second {
		t.Errorf("duration form ignored: %v", s.SidecarIdleTimeout)
	}
	t.Setenv("DY_SIDECAR_IDLE_TIMEOUT", "bogus")
	if s := Load(); s.SidecarIdleTimeout != 10*time.Minute {
		t.Errorf("garbage must fall back to default, got %v", s.SidecarIdleTimeout)
	}
}
