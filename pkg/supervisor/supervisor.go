// Package supervisor manages the lifecycle of a single MCP server process.
// It mirrors systemd's RestartSec + KillSignal semantics: SIGTERM first,
// SIGKILL after grace, restart on unexpected exit.
package supervisor

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/surgifai-com/mcprt/pkg/manifest"
)

// State represents the lifecycle state of a supervised server.
type State string

const (
	StateIdle     State = "idle"
	StateSpawning State = "spawning"
	StateRunning  State = "running"
	StateStopping State = "stopping"
)

// StateChangeFunc is called whenever the server's State changes.
type StateChangeFunc func(name string, from, to State)

// Supervisor manages one MCP server process.
type Supervisor struct {
	name    string
	spec    manifest.ServerSpec
	port    int
	logDir  string
	logger  *slog.Logger
	onState StateChangeFunc

	mu           sync.Mutex
	state        State
	cmd          *exec.Cmd
	restartCount int
	lastSpawn    time.Time
	lastErr      string

	stopCh chan struct{}

	healthTimeout  time.Duration
	killGrace      time.Duration
	totalRequests  atomic.Int64
	totalErrors    atomic.Int64
}

// Options configures a Supervisor.
type Options struct {
	LogDir        string
	Logger        *slog.Logger
	OnStateChange StateChangeFunc
	HealthTimeout time.Duration
	KillGrace     time.Duration
}

// New creates a Supervisor for the named server. port is the dynamically
// allocated port mcprt will tell the server to bind on.
func New(name string, spec manifest.ServerSpec, port int, opts Options) *Supervisor {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	ht := opts.HealthTimeout
	if ht == 0 {
		ht = 5 * time.Second
	}
	kg := opts.KillGrace
	if kg == 0 {
		kg = 5 * time.Second
	}
	return &Supervisor{
		name:          name,
		spec:          spec,
		port:          port,
		logDir:        opts.LogDir,
		logger:        logger.With("server", name),
		onState:       opts.OnStateChange,
		state:         StateIdle,
		healthTimeout: ht,
		killGrace:     kg,
	}
}

// Port returns the port assigned to this server.
func (s *Supervisor) Port() int { return s.port }

// State returns the current lifecycle state.
func (s *Supervisor) CurrentState() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Stats returns a snapshot of runtime metrics.
func (s *Supervisor) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	pid := 0
	if s.cmd != nil && s.cmd.Process != nil {
		pid = s.cmd.Process.Pid
	}
	return Stats{
		Name:         s.name,
		State:        s.state,
		Port:         s.port,
		PID:          pid,
		RestartCount: s.restartCount,
		LastSpawn:    s.lastSpawn,
		LastError:    s.lastErr,
		Requests:     s.totalRequests.Load(),
		Errors:       s.totalErrors.Load(),
	}
}

// IncrRequests increments the served-request counter (called by the proxy).
func (s *Supervisor) IncrRequests() { s.totalRequests.Add(1) }

// IncrErrors increments the error counter (called by the proxy).
func (s *Supervisor) IncrErrors() { s.totalErrors.Add(1) }

// Start spawns the process and waits for it to pass the health check.
// ctx governs the health-check wait only; the process lifetime extends
// until Stop is called.
func (s *Supervisor) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.state != StateIdle {
		s.mu.Unlock()
		return fmt.Errorf("server %q is already %s", s.name, s.state)
	}
	s.setState(StateSpawning)
	s.mu.Unlock()

	cmd, err := s.buildCmd()
	if err != nil {
		s.mu.Lock()
		s.setState(StateIdle)
		s.lastErr = err.Error()
		s.mu.Unlock()
		return err
	}

	if err := cmd.Start(); err != nil {
		s.mu.Lock()
		s.setState(StateIdle)
		s.lastErr = err.Error()
		s.mu.Unlock()
		return fmt.Errorf("spawning %q: %w", s.name, err)
	}

	s.mu.Lock()
	s.cmd = cmd
	s.lastSpawn = time.Now()
	s.stopCh = make(chan struct{})
	s.mu.Unlock()

	// Single cmd.Wait() goroutine — its channel is shared between the health-check
	// race below and watch() so Wait is only called once.
	exitCh := make(chan error, 1)
	go func() { exitCh <- cmd.Wait() }()

	healthCh := make(chan error, 1)
	go func() { healthCh <- s.waitHealthy(ctx) }()

	select {
	case exitErr := <-exitCh:
		// Process died before health check passed.
		exitCode := -1
		if cmd.ProcessState != nil {
			exitCode = cmd.ProcessState.ExitCode()
		}
		s.logger.Error("process exited during startup",
			"exit_code", exitCode, "err", exitErr)
		msg := fmt.Sprintf("exited during startup (code %d): %v", exitCode, exitErr)
		s.mu.Lock()
		s.cmd = nil
		s.setState(StateIdle)
		s.lastErr = msg
		s.mu.Unlock()
		close(s.stopCh)
		return fmt.Errorf("%s", msg)

	case healthErr := <-healthCh:
		if healthErr != nil {
			_ = s.killProcess(cmd)
			// Drain exitCh so the goroutine doesn't leak.
			go func() { <-exitCh }()
			s.mu.Lock()
			s.cmd = nil
			s.setState(StateIdle)
			s.lastErr = healthErr.Error()
			s.mu.Unlock()
			return fmt.Errorf("health check for %q: %w", s.name, healthErr)
		}
	}

	s.mu.Lock()
	s.setState(StateRunning)
	s.mu.Unlock()

	// Pass the already-running exitCh to watch so it doesn't re-Wait.
	go s.watch(cmd, exitCh)

	return nil
}

// Stop sends SIGTERM to the process. If it doesn't exit within killGrace,
// SIGKILL is sent. Blocks until the process exits.
func (s *Supervisor) Stop() error {
	s.mu.Lock()
	if s.state != StateRunning {
		s.mu.Unlock()
		return nil
	}
	s.setState(StateStopping)
	cmd := s.cmd
	stopCh := s.stopCh
	s.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return nil
	}

	s.logger.Info("sending SIGTERM")
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		s.logger.Warn("SIGTERM failed, escalating to SIGKILL", "err", err)
		_ = cmd.Process.Kill()
	}

	select {
	case <-stopCh:
		// Process exited cleanly.
	case <-time.After(s.killGrace):
		s.logger.Warn("grace period expired, sending SIGKILL")
		_ = cmd.Process.Kill()
		<-stopCh
	}
	return nil
}

// UpdateSpec replaces the server spec (called on hot-reload for mutated servers).
// The running process must be stopped first.
func (s *Supervisor) UpdateSpec(spec manifest.ServerSpec) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.spec = spec
}

// --- internals ---

func (s *Supervisor) setState(st State) {
	// Caller holds s.mu.
	prev := s.state
	s.state = st
	if s.onState != nil && prev != st {
		go s.onState(s.name, prev, st)
	}
}

func (s *Supervisor) buildCmd() (*exec.Cmd, error) {
	spec := s.spec
	if len(spec.Exec) == 0 {
		return nil, fmt.Errorf("exec is empty for server %q", s.name)
	}

	argv := make([]string, 0, len(spec.Exec)+len(spec.Args))
	argv = append(argv, spec.Exec...)
	argv = append(argv, spec.Args...)

	// Substitute ${MCPRT_PORT} everywhere in argv.
	portStr := strconv.Itoa(s.port)
	for i, a := range argv {
		argv[i] = strings.ReplaceAll(a, "${MCPRT_PORT}", portStr)
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	if spec.WorkingDir != "" {
		cmd.Dir = spec.WorkingDir
	}

	// Build environment: inherit OS env, then overlay spec.env.
	// ${MCPRT_PORT} is substituted in env values too (for servers that read
	// their port from an env var rather than a CLI flag, e.g. PORT=${MCPRT_PORT}).
	cmd.Env = os.Environ()
	for k, v := range spec.Env {
		v = strings.ReplaceAll(v, "${MCPRT_PORT}", portStr)
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	// Always inject MCPRT_PORT directly so servers can reference it without
	// needing a manifest env entry.
	cmd.Env = append(cmd.Env, "MCPRT_PORT="+portStr)

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd, nil
}

func (s *Supervisor) waitHealthy(ctx context.Context) error {
	switch s.spec.HealthType {
	case "tcp":
		return s.waitHealthyTCP(ctx)
	case "none", "":
		if s.spec.HealthPath == "" {
			// No probe configured — brief fixed delay then assume up.
			select {
			case <-time.After(500 * time.Millisecond):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return s.waitHealthyHTTP(ctx)
	default:
		return s.waitHealthyHTTP(ctx)
	}
}

func (s *Supervisor) waitHealthyTCP(ctx context.Context) error {
	addr := fmt.Sprintf("127.0.0.1:%d", s.port)
	deadline := time.Now().Add(s.healthTimeout)
	dialer := &net.Dialer{Timeout: 300 * time.Millisecond}

	for {
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err == nil {
			conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for TCP %s after %s", addr, s.healthTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (s *Supervisor) waitHealthyHTTP(ctx context.Context) error {
	url := fmt.Sprintf("http://127.0.0.1:%d%s", s.port, s.spec.HealthPath)
	deadline := time.Now().Add(s.healthTimeout)
	client := &http.Client{Timeout: 500 * time.Millisecond}

	for {
		resp, err := client.Get(url)
		if err == nil && resp.StatusCode < 500 {
			resp.Body.Close()
			return nil
		}
		if resp != nil {
			resp.Body.Close()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %s after %s", url, s.healthTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (s *Supervisor) watch(cmd *exec.Cmd, exitCh <-chan error) {
	err := <-exitCh

	s.mu.Lock()
	stopCh := s.stopCh
	wasRunning := s.state == StateRunning
	if wasRunning {
		s.restartCount++
		if err != nil {
			s.lastErr = err.Error()
		}
	}
	s.cmd = nil
	s.setState(StateIdle)
	s.mu.Unlock()

	if stopCh != nil {
		close(stopCh)
	}

	if wasRunning {
		s.logger.Warn("process exited unexpectedly", "err", err)
	} else {
		s.logger.Info("process exited", "err", err)
	}
}

func (s *Supervisor) killProcess(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

// Stats is a point-in-time snapshot of a supervised server's metrics.
type Stats struct {
	Name         string
	State        State
	Port         int
	PID          int
	RestartCount int
	LastSpawn    time.Time
	LastError    string
	Requests     int64
	Errors       int64
}
