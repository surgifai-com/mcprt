// Package runtime is the public library API for mcprt.
// It wires together the manifest loader, policy validator, supervisor registry,
// proxy handler, and fsnotify hot-reload loop.
package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/surgifai-com/mcprt/pkg/manifest"
	"github.com/surgifai-com/mcprt/pkg/policy"
	"github.com/surgifai-com/mcprt/pkg/proxy"
	"github.com/surgifai-com/mcprt/pkg/supervisor"
)

// Runtime is the central coordinator.
type Runtime struct {
	cfg         *manifest.Config
	manifestPath string
	logger      *slog.Logger

	mu       sync.RWMutex
	sups     map[string]*supervisor.Supervisor
	refs     map[string]*proxy.RefCounter
	ports    map[string]int
	nextPort int

	handler *proxy.Handler

	// Lifecycle hooks for embedders.
	OnSpawn         func(name string)
	OnExit          func(name string)
	OnRefcountChange func(name string, count int)
}

// Options configures the runtime.
type Options struct {
	ManifestPath string
	Logger       *slog.Logger
	LogDir       string
	BasePort     int // first port to allocate from (default 19000)
}

// New loads the manifest, validates it, and returns a configured Runtime.
// It does NOT start any servers or listen on any port yet.
func New(opts Options) (*Runtime, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if opts.ManifestPath == "" {
		home, _ := os.UserHomeDir()
		opts.ManifestPath = filepath.Join(home, ".config", "mcprt", "mcprt.toml")
	}

	cfg, err := manifest.Load(opts.ManifestPath)
	if err != nil {
		return nil, err
	}
	vv := policy.Validate(cfg)
	if policy.HasErrors(vv) {
		for _, v := range vv {
			if v.Severity == policy.SeverityError {
				logger.Error("policy violation", "rule", v.Rule, "server", v.Server, "msg", v.Message)
			}
		}
		return nil, fmt.Errorf("manifest has policy errors — see above")
	}
	for _, v := range vv {
		if v.Severity == policy.SeverityWarning {
			logger.Warn("policy warning", "rule", v.Rule, "server", v.Server, "msg", v.Message)
		}
	}

	base := opts.BasePort
	if base == 0 {
		base = 19000
	}

	r := &Runtime{
		cfg:          cfg,
		manifestPath: opts.ManifestPath,
		logger:       logger,
		sups:         make(map[string]*supervisor.Supervisor),
		refs:         make(map[string]*proxy.RefCounter),
		ports:        make(map[string]int),
		nextPort:     base,
		handler:      proxy.NewHandler(logger, 10*time.Second),
	}

	gracePeriod, err := time.ParseDuration(cfg.Runtime.GracePeriod)
	if err != nil {
		gracePeriod = 5 * time.Second
	}

	for name, spec := range cfg.Server {
		r.registerServer(name, spec, gracePeriod, opts.LogDir)
	}

	return r, nil
}

// Serve starts the HTTP listener and blocks until ctx is cancelled.
func (r *Runtime) Serve(ctx context.Context) error {
	srv := &http.Server{
		Addr:    r.cfg.Runtime.Listen,
		Handler: r.handler,
	}

	ln, err := net.Listen("tcp", r.cfg.Runtime.Listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", r.cfg.Runtime.Listen, err)
	}
	r.logger.Info("mcprt listening", "addr", r.cfg.Runtime.Listen)

	// Start fsnotify hot-reload.
	go r.watchManifest(ctx)

	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		r.logger.Info("shutting down")
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)

		// Stop all running servers.
		r.mu.RLock()
		names := make([]string, 0, len(r.sups))
		for n := range r.sups {
			names = append(names, n)
		}
		r.mu.RUnlock()
		for _, n := range names {
			r.mu.RLock()
			sup := r.sups[n]
			r.mu.RUnlock()
			_ = sup.Stop()
		}
		return nil
	}
}

// AllStats returns a snapshot of all registered servers.
func (r *Runtime) AllStats() []supervisor.Stats {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]supervisor.Stats, 0, len(r.sups))
	for _, sup := range r.sups {
		out = append(out, sup.Stats())
	}
	return out
}

// --- internal ---

func (r *Runtime) registerServer(name string, spec manifest.ServerSpec, gracePeriod time.Duration, logDir string) {
	port := r.allocPort(name, spec)

	ref := proxy.NewRefCounter(gracePeriod, func() {
		r.mu.RLock()
		sup, ok := r.sups[name]
		r.mu.RUnlock()
		if !ok {
			return
		}
		if sup.CurrentState() == supervisor.StateRunning {
			r.logger.Info("refcount debounce expired, stopping server", "server", name)
			if err := sup.Stop(); err != nil {
				r.logger.Error("stop error", "server", name, "err", err)
			}
			if r.OnExit != nil {
				go r.OnExit(name)
			}
		}
	})

	var healthTimeout time.Duration
	if spec.HealthTimeout != "" {
		if d, err := time.ParseDuration(spec.HealthTimeout); err == nil {
			healthTimeout = d
		}
	}

	sup := supervisor.New(name, spec, port, supervisor.Options{
		LogDir:        logDir,
		Logger:        r.logger,
		HealthTimeout: healthTimeout,
		OnStateChange: func(n string, from, to supervisor.State) {
			r.logger.Info("state change", "server", n, "from", from, "to", to)
			if to == supervisor.StateRunning && r.OnSpawn != nil {
				go r.OnSpawn(n)
			}
		},
	})

	r.mu.Lock()
	r.sups[name] = sup
	r.refs[name] = ref
	r.mu.Unlock()

	r.handler.Register(name, &proxy.ServerEntry{Sup: sup, Ref: ref, SpawnTimeout: healthTimeout})
}

func (r *Runtime) allocPort(name string, spec manifest.ServerSpec) int {
	if spec.Port > 0 {
		r.ports[name] = spec.Port
		return spec.Port
	}
	if p, ok := r.ports[name]; ok {
		return p
	}
	p := r.nextPort
	r.nextPort++
	r.ports[name] = p
	return p
}

func (r *Runtime) watchManifest(ctx context.Context) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		r.logger.Error("fsnotify init failed", "err", err)
		return
	}
	defer watcher.Close()

	if err := watcher.Add(r.manifestPath); err != nil {
		r.logger.Error("fsnotify watch failed", "err", err, "path", r.manifestPath)
		return
	}

	var debounce *time.Timer
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
				if debounce != nil {
					debounce.Stop()
				}
				debounce = time.AfterFunc(500*time.Millisecond, func() {
					r.reload()
				})
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			r.logger.Error("fsnotify error", "err", err)
		}
	}
}

func (r *Runtime) reload() {
	cfg, err := manifest.Load(r.manifestPath)
	if err != nil {
		r.logger.Error("hot-reload: parse failed", "err", err)
		return
	}
	vv := policy.Validate(cfg)
	if policy.HasErrors(vv) {
		for _, v := range vv {
			if v.Severity == policy.SeverityError {
				r.logger.Error("hot-reload: policy error — refusing reload", "rule", v.Rule, "server", v.Server, "msg", v.Message)
			}
		}
		return
	}
	for _, v := range vv {
		if v.Severity == policy.SeverityWarning {
			r.logger.Warn("hot-reload: policy warning", "rule", v.Rule, "server", v.Server, "msg", v.Message)
		}
	}

	gracePeriod, _ := time.ParseDuration(cfg.Runtime.GracePeriod)
	if gracePeriod == 0 {
		gracePeriod = 5 * time.Second
	}

	r.mu.RLock()
	existing := make(map[string]bool, len(r.sups))
	for n := range r.sups {
		existing[n] = true
	}
	r.mu.RUnlock()

	// Added / mutated servers.
	for name, spec := range cfg.Server {
		r.mu.RLock()
		sup, alreadyExists := r.sups[name]
		r.mu.RUnlock()

		if !alreadyExists {
			r.logger.Info("hot-reload: adding server", "server", name)
			r.registerServer(name, spec, gracePeriod, "")
		} else {
			// Mutated: only restart if currently running.
			if sup.CurrentState() == supervisor.StateRunning {
				r.logger.Info("hot-reload: spec changed, restarting running server", "server", name)
				_ = sup.Stop()
				sup.UpdateSpec(spec)
				// Server will restart on next client connection (refcount-driven).
			} else {
				sup.UpdateSpec(spec)
			}
			delete(existing, name)
		}
	}

	// Removed servers: stop and deregister.
	for name := range existing {
		r.logger.Info("hot-reload: removing server", "server", name)
		r.mu.RLock()
		sup := r.sups[name]
		r.mu.RUnlock()
		_ = sup.Stop()
		r.mu.Lock()
		delete(r.sups, name)
		delete(r.refs, name)
		r.mu.Unlock()
		r.handler.Deregister(name)
	}

	r.mu.Lock()
	r.cfg = cfg
	r.mu.Unlock()
	r.logger.Info("hot-reload: complete")
}
