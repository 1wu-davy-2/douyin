// Package sidecar manages the lifecycle of the Python signing sidecar process
// (sidecar/main.py): idempotent start, health polling, idle-timeout kill and
// graceful shutdown.
//
// The Manager only owns the process lifecycle — business call timeouts are the
// provider layer's responsibility (a real /posts call can legitimately take up
// to ~65s because of F2-internal retries).
package sidecar

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const (
	stateStopped  = "stopped"
	stateStarting = "starting"
	stateRunning  = "running"

	// healthTimeout is the per-poll HTTP timeout while waiting for readiness.
	healthTimeout = 2 * time.Second
	// startupDeadline bounds the total readiness wait (contract: 25s).
	startupDeadline = 25 * time.Second
	// startupPollInterval is the readiness poll cadence.
	startupPollInterval = 200 * time.Millisecond
	// sidecarScript is launched relative to the server cwd (repo root per
	// project convention).
	sidecarScript = "sidecar/main.py"
)

// Endpoint is the subset of Manager the provider layer uses. It exists so the
// provider can be unit-tested against a stub HTTP server.
type Endpoint interface {
	Ensure(ctx context.Context) error
	BaseURL() string
	Token() string
	Touch()
}

// attempt is one start-attempt shared by all concurrent Ensure callers: the
// goroutine writes err then closes done, which broadcasts the result.
type attempt struct {
	done chan struct{}
	err  error
}

// Manager keeps track of one sidecar process.
type Manager struct {
	port        int
	pythonPath  string
	mock        bool
	idleTimeout atomic.Int64 // nanoseconds

	mu       sync.Mutex
	state    string
	pid      int
	gen      int
	token    string
	baseURL  string
	attempt  *attempt
	onStatus func(state string)

	lastUse  atomic.Int64 // nanos of last Touch
	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
	stdout   io.Writer
}

// NewManager creates the manager; the idle reaper goroutine starts immediately
// and runs until Stop.
func NewManager(port int, pythonPath string, mock bool, idleTimeout time.Duration) *Manager {
	m := &Manager{
		port:       port,
		pythonPath: pythonPath,
		mock:       mock,
		stopCh:     make(chan struct{}),
		state:      stateStopped,
		baseURL:    fmt.Sprintf("http://127.0.0.1:%d", port),
		stdout:     os.Stderr,
	}
	m.idleTimeout.Store(int64(idleTimeout))
	m.lastUse.Store(time.Now().UnixNano())

	m.wg.Add(1)
	go m.idleReaper()
	return m
}

// SetOnStatus registers a callback fired (asynchronously) on every state
// transition. Set it before the first Ensure.
func (m *Manager) SetOnStatus(fn func(state string)) {
	m.mu.Lock()
	m.onStatus = fn
	m.mu.Unlock()
}

// SetIdleTimeout adjusts the idle timeout at runtime (settings integration).
func (m *Manager) SetIdleTimeout(d time.Duration) {
	if d > 0 {
		m.idleTimeout.Store(int64(d))
	}
}

// Touch marks the sidecar as "in use right now", postponing the idle kill.
// The provider calls it around every business request.
func (m *Manager) Touch() {
	m.lastUse.Store(time.Now().UnixNano())
}

// Status returns the current lifecycle state: stopped | starting | running.
func (m *Manager) Status() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// BaseURL returns the sidecar base URL (http://127.0.0.1:<port>).
func (m *Manager) BaseURL() string { return m.baseURL }

// Token returns the token of the current (or most recent) process.
func (m *Manager) Token() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.token
}

// Ensure starts the sidecar if needed and blocks until it is healthy. It is
// idempotent and safe for concurrent callers (they wait on the same attempt).
// ctx bounds only the *wait*; a running start attempt continues until it
// becomes healthy or exhausts its startup deadline.
func (m *Manager) Ensure(ctx context.Context) error {
	m.Touch()
	m.mu.Lock()
	switch m.state {
	case stateRunning:
		m.mu.Unlock()
		return nil
	case stateStarting:
		att := m.attempt
		m.mu.Unlock()
		return m.waitAttempt(ctx, att)
	default: // stopped
		m.state = stateStarting
		m.notifyStatusLocked(stateStarting)
		att := &attempt{done: make(chan struct{})}
		m.attempt = att
		m.gen++
		gen := m.gen
		token := newSidecarToken()
		go m.startAttempt(gen, token, att)
		m.mu.Unlock()
		return m.waitAttempt(ctx, att)
	}
}

// waitAttempt blocks for the outcome of a start attempt, honoring ctx.
func (m *Manager) waitAttempt(ctx context.Context, att *attempt) error {
	select {
	case <-att.done:
		return att.err
	case <-ctx.Done():
		return fmt.Errorf("sidecar: wait for readiness canceled: %w", ctx.Err())
	}
}

// finishAttempt publishes the attempt outcome to every waiter.
func finishAttempt(att *attempt, err error) {
	att.err = err
	close(att.done)
}

// startAttempt launches the python process and polls /health until it answers
// (or the startup deadline / shutdown / process death intervenes).
func (m *Manager) startAttempt(gen int, token string, att *attempt) {
	cmd := exec.Command(m.pythonPath, sidecarScript,
		"--port", strconv.Itoa(m.port),
		"--token", token,
	)
	if m.mock {
		cmd.Args = append(cmd.Args, "--mock")
	}
	// Inherit the environment so DY_DATA_DIR reaches the sidecar's cookie reader.
	cmd.Stdout = m.stdout
	cmd.Stderr = m.stdout
	// Own process group on Unix so the whole tree can be killed at once.
	setNewProcessGroup(cmd)

	if err := cmd.Start(); err != nil {
		m.failAttempt(gen, att, fmt.Errorf("sidecar: start %s: %w", m.pythonPath, err))
		return
	}
	pid := cmd.Process.Pid

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	m.mu.Lock()
	m.pid = pid
	m.token = token
	m.mu.Unlock()
	log.Printf("[sidecar] starting pid=%d port=%d mock=%v python=%s", pid, m.port, m.mock, m.pythonPath)

	client := &http.Client{Timeout: healthTimeout}
	deadline := time.Now().Add(startupDeadline)
	for {
		select {
		case <-m.stopCh:
			killProcessTree(pid)
			m.failAttempt(gen, att, errors.New("sidecar: startup aborted (server shutting down)"))
			return
		case waitErr := <-waitCh:
			m.failAttempt(gen, att, fmt.Errorf("sidecar: process exited during startup: %v", waitErr))
			return
		case <-time.After(startupPollInterval):
		}

		if m.healthy(client, token) {
			m.mu.Lock()
			m.state = stateRunning
			m.notifyStatusLocked(stateRunning)
			m.mu.Unlock()
			log.Printf("[sidecar] ready pid=%d", pid)
			finishAttempt(att, nil)

			// Crash watcher: mark stopped if the process dies later; the next
			// Ensure restarts it. Exactly one goroutine consumes waitCh — the
			// startup poll returned before receiving from it.
			m.wg.Add(1)
			go func() {
				defer m.wg.Done()
				waitErr := <-waitCh
				log.Printf("[sidecar] process exited pid=%d: %v", pid, waitErr)
				m.mu.Lock()
				if m.gen == gen && (m.state == stateRunning || m.state == stateStarting) {
					m.state = stateStopped
					m.notifyStatusLocked(stateStopped)
				}
				m.mu.Unlock()
			}()
			return
		}

		if time.Now().After(deadline) {
			killProcessTree(pid)
			m.failAttempt(gen, att, fmt.Errorf("sidecar: not healthy within %s", startupDeadline))
			return
		}
	}
}

// healthy probes GET /health with the sidecar token.
func (m *Manager) healthy(client *http.Client, token string) bool {
	req, err := http.NewRequest(http.MethodGet, m.baseURL+"/health", nil)
	if err != nil {
		return false
	}
	req.Header.Set("X-Sidecar-Token", token)
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	return resp.StatusCode == http.StatusOK
}

// failAttempt logs the failure, returns the manager to stopped state and
// publishes the outcome.
func (m *Manager) failAttempt(gen int, att *attempt, err error) {
	log.Printf("[sidecar] %v", err)
	m.mu.Lock()
	if m.gen == gen {
		m.state = stateStopped
		m.notifyStatusLocked(stateStopped)
	}
	m.mu.Unlock()
	finishAttempt(att, err)
}

// idleReaper kills the sidecar after the idle timeout elapsed since the last
// Touch; Ensure restarts it on the next business call.
func (m *Manager) idleReaper() {
	defer m.wg.Done()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.mu.Lock()
			running := m.state == stateRunning
			pid := m.pid
			gen := m.gen
			m.mu.Unlock()
			if !running {
				continue
			}
			timeout := time.Duration(m.idleTimeout.Load())
			if timeout > 0 && time.Since(time.Unix(0, m.lastUse.Load())) > timeout {
				log.Printf("[sidecar] idle for > %s, killing pid=%d", timeout, pid)
				killProcessTree(pid)
				// Confirm the port is actually free before flipping to stopped:
				// a kill that silently failed (e.g. missing binary, EPERM) must
				// not be mistaken for success — the orphan would keep holding
				// the port and every restart would die with "Address already in
				// use" until the container is recreated.
				if !m.awaitDeath(gen) {
					log.Printf("[sidecar] port %d still listening after kill (pid=%d); keeping state, retrying next tick", m.port, pid)
					continue
				}
				m.mu.Lock()
				if m.gen == gen && m.state == stateRunning {
					m.state = stateStopped
					m.notifyStatusLocked(stateStopped)
				}
				m.mu.Unlock()
			}
		}
	}
}

// Stop shuts the sidecar down (killing the whole process tree on Windows) and
// stops all manager goroutines. It is safe to call multiple times.
func (m *Manager) Stop() {
	m.stopOnce.Do(func() { close(m.stopCh) })

	m.mu.Lock()
	state := m.state
	pid := m.pid
	m.mu.Unlock()

	if state == stateRunning || state == stateStarting {
		killProcessTree(pid)
	}
	m.wg.Wait()

	m.mu.Lock()
	m.state = stateStopped
	m.pid = 0
	m.mu.Unlock()
	log.Printf("[sidecar] stopped")
}

// awaitDeath polls until the sidecar port stops accepting connections (up to
// ~3s) and reports whether the port is free — the process is presumed dead.
// If the generation changed or the state moved on meanwhile (e.g. the crash
// watcher already flipped to stopped and a new sidecar started), it reports
// success: there is nothing left to confirm for this generation.
func (m *Manager) awaitDeath(gen int) bool {
	addr := fmt.Sprintf("127.0.0.1:%d", m.port)
	deadline := time.Now().Add(3 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, 250*time.Millisecond)
		if err != nil {
			return true // connection refused: nothing listening anymore
		}
		_ = conn.Close()
		m.mu.Lock()
		superseded := m.gen != gen || m.state != stateRunning
		m.mu.Unlock()
		if superseded {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// notifyStatusLocked fires the status callback asynchronously; caller holds
// m.mu and has already set m.state.
func (m *Manager) notifyStatusLocked(newState string) {
	if fn := m.onStatus; fn != nil {
		go fn(newState)
	}
}

// newSidecarToken generates the random 32-byte shared token passed to the
// sidecar via argv (fresh for every process start).
func newSidecarToken() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Sprintf("sidecar: token generation failed: %v", err))
	}
	return hex.EncodeToString(buf)
}
