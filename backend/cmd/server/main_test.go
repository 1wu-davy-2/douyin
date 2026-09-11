package main

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"douyin/backend/internal/config"
)

// run must serve until its context is canceled, then release everything and
// return nil (graceful shutdown: HTTP -> sidecar -> db).
func TestRunGracefulShutdown(t *testing.T) {
	cfg := config.Settings{
		Port:               18877,
		DataDir:            t.TempDir(),
		DBPath:             filepath.Join(t.TempDir(), "run.db"),
		Mock:               true,
		SidecarPort:        18797,
		SidecarPython:      "python",
		SidecarIdleTimeout: 10 * time.Minute,
	}

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- run(ctx, cfg) }()

	// Wait until the server answers.
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(15 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://127.0.0.1:18877/api/health")
		if err == nil {
			resp.Body.Close()
			ready = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		t.Fatal("server did not become ready")
	}

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("run returned error on graceful shutdown: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return after context cancel")
	}

	// The listener must be closed after run returns.
	if conn, err := net.DialTimeout("tcp", "127.0.0.1:18877", time.Second); err == nil {
		conn.Close()
		t.Fatal("port still accepting connections after shutdown")
	}
}
