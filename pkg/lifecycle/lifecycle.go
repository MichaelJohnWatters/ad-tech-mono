// Package lifecycle provides graceful shutdown for all services.
//
// Every service uses this to handle SIGTERM/SIGINT cleanly:
// drain in-flight requests, flush buffers, ack pending messages,
// then exit.
//
// Usage:
//
//	lc := lifecycle.New(logger)
//	lc.OnShutdown("flush-nats", func(ctx context.Context) error {
//	    return natsConn.Drain()
//	})
//	lc.OnShutdown("close-db", func(ctx context.Context) error {
//	    return db.Close()
//	})
//	// Blocks until SIGTERM/SIGINT, then runs shutdown hooks
//	lc.Wait(30 * time.Second)
package lifecycle

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// ShutdownFunc is called during graceful shutdown.
// It receives a context with a deadline (the grace period).
type ShutdownFunc func(ctx context.Context) error

type hook struct {
	name string
	fn   ShutdownFunc
}

// Lifecycle manages graceful startup and shutdown.
type Lifecycle struct {
	log   *slog.Logger
	mu    sync.Mutex
	hooks []hook
}

// New creates a Lifecycle with the given logger.
func New(log *slog.Logger) *Lifecycle {
	return &Lifecycle{
		log: log,
	}
}

// OnShutdown registers a named shutdown hook.
// Hooks run in the order they were registered.
func (lc *Lifecycle) OnShutdown(name string, fn ShutdownFunc) {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	lc.hooks = append(lc.hooks, hook{name: name, fn: fn})
}

// OnShutdownFirst PREPENDS a hook so it runs before everything registered so
// far. Exists for the HTTP drain: services wire their dependencies (NATS,
// DB) and register those closes BEFORE lifecycle.ServeHTTP registers the
// server shutdown — so in registration order, a draining pod closed its bus
// while in-flight requests were still publishing (observed live 2026-07-19:
// an SSP replica lost 3 behaviour events during a mid-load rolling restart,
// "nats: connection closed"). Drain-then-teardown is the only correct order.
func (lc *Lifecycle) OnShutdownFirst(name string, fn ShutdownFunc) {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	lc.hooks = append([]hook{{name: name, fn: fn}}, lc.hooks...)
}

// Wait blocks until SIGTERM or SIGINT is received, then runs all
// shutdown hooks within the given grace period.
// Returns an error if any hook fails.
func (lc *Lifecycle) Wait(gracePeriod time.Duration) error {
	// Wait for shutdown signal
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)

	received := <-sig
	lc.log.Info("shutdown signal received", "signal", received.String())

	return lc.Shutdown(gracePeriod)
}

// Shutdown runs all registered hooks within the grace period.
// Can be called directly for testing (instead of Wait which blocks on signals).
func (lc *Lifecycle) Shutdown(gracePeriod time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), gracePeriod)
	defer cancel()

	lc.mu.Lock()
	hooks := make([]hook, len(lc.hooks))
	copy(hooks, lc.hooks)
	lc.mu.Unlock()

	var errs []error

	for _, h := range hooks {
		lc.log.Info("running shutdown hook", "hook", h.name)
		if err := h.fn(ctx); err != nil {
			lc.log.Error("shutdown hook failed", "hook", h.name, "error", err)
			errs = append(errs, fmt.Errorf("%s: %w", h.name, err))
		} else {
			lc.log.Info("shutdown hook completed", "hook", h.name)
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("shutdown errors: %v", errs)
	}

	lc.log.Info("graceful shutdown complete")
	return nil
}
