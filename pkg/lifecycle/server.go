package lifecycle

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"syscall"
	"time"
)

// listenReuseAddr creates a TCP listener with SO_REUSEADDR.
// This allows rebind when the old socket is in TIME_WAIT state.
func listenReuseAddr(addr string) (net.Listener, error) {
	lc := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			var opErr error
			if err := c.Control(func(fd uintptr) {
				opErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
			}); err != nil {
				return err
			}
			return opErr
		},
	}
	return lc.Listen(context.Background(), "tcp", addr)
}

// ListenAndServeWithRetry attempts to start the HTTP server with SO_REUSEADDR,
// retrying if the port is temporarily unavailable.
func ListenAndServeWithRetry(server *http.Server, log *slog.Logger, maxRetries int, retryDelay time.Duration) error {
	var lastErr error
	for i := 0; i <= maxRetries; i++ {
		listener, err := listenReuseAddr(server.Addr)
		if err != nil {
			lastErr = err
			if i < maxRetries {
				log.Warn("port in use, retrying",
					"addr", server.Addr,
					"attempt", i+1,
					"max_retries", maxRetries,
				)
				time.Sleep(retryDelay)
				continue
			}
			return fmt.Errorf("failed to bind after %d attempts: %w", maxRetries+1, lastErr)
		}
		if i > 0 {
			log.Info("port acquired after retry", "addr", server.Addr, "attempts", i+1)
		} else {
			log.Info("server listening", "addr", server.Addr)
		}
		return server.Serve(listener)
	}
	return lastErr
}

// ServeHTTP is a helper that starts an HTTP server with retry and registers
// shutdown with the lifecycle manager. Blocks until shutdown signal.
func ServeHTTP(lc *Lifecycle, server *http.Server, log *slog.Logger, gracePeriod time.Duration) error {
	lc.OnShutdown("http-server", func(ctx context.Context) error {
		return server.Shutdown(ctx)
	})

	go func() {
		if err := ListenAndServeWithRetry(server, log, 30, 1*time.Second); err != nil && err != http.ErrServerClosed {
			log.Error("server failed to start", "error", err)
		}
	}()

	return lc.Wait(gracePeriod)
}
