package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/victorqnguyen/mcprt/pkg/supervisor"
)

// ServerEntry bundles a supervisor with its refcounter.
type ServerEntry struct {
	Sup          *supervisor.Supervisor
	Ref          *RefCounter
	SpawnTimeout time.Duration // overrides Handler.spawnTimeout for this server
	name         string
	logger       *slog.Logger
}

// Handler is the HTTP handler for the mcprt proxy. It routes on the first
// path segment: /vault-mcp/... → supervisor for "vault-mcp", etc.
type Handler struct {
	mu      sync.RWMutex
	servers map[string]*ServerEntry
	logger  *slog.Logger

	// spawnTimeout is how long we wait for a cold-start before returning 503.
	spawnTimeout time.Duration
}

// NewHandler creates an empty proxy handler.
func NewHandler(logger *slog.Logger, spawnTimeout time.Duration) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	if spawnTimeout == 0 {
		spawnTimeout = 10 * time.Second
	}
	return &Handler{
		servers:      make(map[string]*ServerEntry),
		logger:       logger,
		spawnTimeout: spawnTimeout,
	}
}

// Register adds a supervised server to the proxy routing table.
func (h *Handler) Register(name string, entry *ServerEntry) {
	entry.name = name
	entry.logger = h.logger.With("server", name)
	h.mu.Lock()
	h.servers[name] = entry
	h.mu.Unlock()
}

// Deregister removes a server from the routing table.
func (h *Handler) Deregister(name string) {
	h.mu.Lock()
	delete(h.servers, name)
	h.mu.Unlock()
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name, tail := splitPath(r.URL.Path)
	if name == "" {
		http.Error(w, "mcprt: missing server name in path (expected /<server>/...)", http.StatusNotFound)
		return
	}

	h.mu.RLock()
	entry, ok := h.servers[name]
	h.mu.RUnlock()

	if !ok {
		http.Error(w, fmt.Sprintf("mcprt: unknown server %q", name), http.StatusNotFound)
		return
	}

	// If the server is idle, spawn it and wait for it to become healthy.
	if entry.Sup.CurrentState() == supervisor.StateIdle {
		entry.logger.Info("cold start triggered", "method", r.Method, "path", tail)
		spawnTimeout := h.spawnTimeout
		if entry.SpawnTimeout > 0 {
			spawnTimeout = entry.SpawnTimeout
		}
		ctx, cancel := context.WithTimeout(r.Context(), spawnTimeout)
		defer cancel()
		if err := entry.Sup.Start(ctx); err != nil {
			entry.logger.Error("spawn failed", "err", err)
			entry.Sup.IncrErrors()
			http.Error(w, fmt.Sprintf("mcprt: server %q failed to start: %v", name, err), http.StatusServiceUnavailable)
			return
		}
	}

	// SSE connections are the primary refcount signal.
	isSSE := isSSERequest(r)
	sessionID := r.Header.Get("Mcp-Session-Id")

	if isSSE {
		entry.Ref.Acquire()
		defer entry.Ref.Release()
	} else if sessionID != "" && !isSSE {
		// Ephemeral session: held until the underlying TCP connection closes.
		// We use CloseNotifier's replacement: context cancellation on request done.
		done := entry.Ref.TrackEphemeral(sessionID)
		go func() {
			<-r.Context().Done()
			done()
		}()
	}

	// Rewrite path: strip /<server-name> prefix.
	r2 := r.Clone(r.Context())
	r2.URL.Path = tail
	if r2.URL.RawPath != "" {
		r2.URL.RawPath = tail
	}

	target := &url.URL{
		Scheme: "http",
		Host:   fmt.Sprintf("127.0.0.1:%d", entry.Sup.Port()),
	}

	rp := httputil.NewSingleHostReverseProxy(target)
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		entry.logger.Error("upstream error", "err", err)
		entry.Sup.IncrErrors()
		http.Error(w, fmt.Sprintf("mcprt: upstream %q error: %v", name, err), http.StatusBadGateway)
	}

	entry.Sup.IncrRequests()
	rp.ServeHTTP(w, r2)
}

// splitPath returns the first path segment and the remainder.
// "/vault-mcp/mcp" → ("vault-mcp", "/mcp")
// "/vault-mcp"     → ("vault-mcp", "/")
func splitPath(p string) (name, tail string) {
	p = strings.TrimPrefix(p, "/")
	idx := strings.IndexByte(p, '/')
	if idx < 0 {
		return p, "/"
	}
	return p[:idx], p[idx:]
}

// isSSERequest returns true for requests that will produce a persistent
// SSE stream (Accept: text/event-stream).
func isSSERequest(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/event-stream")
}
