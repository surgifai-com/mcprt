// Package proxy implements the Streamable HTTP reverse proxy and RefCounter.
package proxy

import (
	"context"
	"sync"
	"time"
)

// RefCounter tracks the number of live client connections for a single server.
// Primary signal: open SSE streams. Secondary signal: ephemeral POST sessions
// tracked by Mcp-Session-Id until their underlying TCP connection closes.
//
// When the count transitions to zero it starts a debounce timer; if the count
// remains at zero for the full grace period the onZero callback is fired.
// This is NOT an idle timeout — it only fires when all connections have closed.
type RefCounter struct {
	mu          sync.Mutex
	count       int
	sessions    map[string]context.CancelFunc // ephemeral sessions
	gracePeriod time.Duration
	onZero      func()
	debounce    *time.Timer
}

// NewRefCounter creates a RefCounter. onZero is called (once, non-blocking)
// when the count has been stable at zero for gracePeriod.
func NewRefCounter(gracePeriod time.Duration, onZero func()) *RefCounter {
	return &RefCounter{
		sessions:    make(map[string]context.CancelFunc),
		gracePeriod: gracePeriod,
		onZero:      onZero,
	}
}

// Count returns the current reference count.
func (r *RefCounter) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count
}

// Acquire increments the refcount (SSE stream opened).
func (r *RefCounter) Acquire() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.count++
	r.cancelDebounce()
}

// Release decrements the refcount (SSE stream closed).
func (r *RefCounter) Release() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.count > 0 {
		r.count--
	}
	if r.count == 0 {
		r.startDebounce()
	}
}

// TrackEphemeral registers an ephemeral session ID (POST without SSE).
// Returns a done func the caller must invoke when the underlying connection closes.
func (r *RefCounter) TrackEphemeral(sessionID string) (done func()) {
	ctx, cancel := context.WithCancel(context.Background())
	r.mu.Lock()
	r.sessions[sessionID] = cancel
	r.count++
	r.cancelDebounce()
	r.mu.Unlock()

	// Start background goroutine that waits for the context to be cancelled
	// (either by the caller's done() or by a future ForgetEphemeral call).
	go func() {
		<-ctx.Done()
		r.mu.Lock()
		delete(r.sessions, sessionID)
		if r.count > 0 {
			r.count--
		}
		if r.count == 0 {
			r.startDebounce()
		}
		r.mu.Unlock()
	}()

	return cancel
}

// ForgetEphemeral cancels a tracked session by ID (e.g., on DELETE /session).
func (r *RefCounter) ForgetEphemeral(sessionID string) {
	r.mu.Lock()
	cancel, ok := r.sessions[sessionID]
	r.mu.Unlock()
	if ok {
		cancel()
	}
}

// --- internal ---

// cancelDebounce must be called with r.mu held.
func (r *RefCounter) cancelDebounce() {
	if r.debounce != nil {
		r.debounce.Stop()
		r.debounce = nil
	}
}

// startDebounce must be called with r.mu held. Fires onZero after gracePeriod
// if the count stays at zero.
func (r *RefCounter) startDebounce() {
	r.cancelDebounce()
	cb := r.onZero
	gp := r.gracePeriod
	r.debounce = time.AfterFunc(gp, func() {
		if cb != nil {
			cb()
		}
	})
}
