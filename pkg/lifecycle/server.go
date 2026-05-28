package lifecycle

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// ListenAndServeWithRetry attempts to start the HTTP server, retrying
// if the port is temporarily in use (e.g. during Tilt restarts).
// Retries up to maxRetries times with retryDelay between attempts.
func ListenAndServeWithRetry(server *http.Server, log *slog.Logger, maxRetries int, retryDelay time.Duration) error {
	var lastErr error
	for i := 0; i <= maxRetries; i++ {
		listener, err := net.Listen("tcp", server.Addr)
		if err != nil {
			lastErr = err
			if i < maxRetries {
				log.Warn("port in use, retrying",
					"addr", server.Addr,
					"attempt", i+1,
					"max_retries", maxRetries,
					"retry_in", retryDelay,
				)
				time.Sleep(retryDelay)
				continue
			}
			return fmt.Errorf("failed to bind after %d attempts: %w", maxRetries+1, lastErr)
		}
		log.Info("server listening", "addr", server.Addr)
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
		if err := ListenAndServeWithRetry(server, log, 10, 2*time.Second); err != nil && err != http.ErrServerClosed {
			log.Error("server error", "error", err)
		}
	}()

	return lc.Wait(gracePeriod)
}
