// Package logger provides structured JSON logging for all services.
//
// Every log line includes: trace_id, service name, timestamp, log level.
// All services use this package for consistent format.
//
// Usage:
//
//	log := logger.New("dsp")
//	log.Info("bid evaluated", "trace_id", traceID, "campaign_id", campID, "bid", 2.50)
//	log.Error("budget check failed", "trace_id", traceID, "error", err)
package logger

import (
	"context"
	"io"
	"log/slog"
	"os"
)

type contextKey string

const traceIDKey contextKey = "trace_id"

// New creates a structured JSON logger for a service.
// All log lines include the service name automatically.
//
// If LOKI_URL is set in the environment, log lines are also asynchronously
// pushed to Loki for centralised viewing in Grafana. Stdout output stays
// the same, so `tilt logs <service>` still works as before.
//
// POD_NAME env var, if set, becomes the `pod` Loki label so the same
// service running multiple pods (internal/competitor1/competitor2 DSPs)
// can be filtered apart in Grafana.
func New(service string) *slog.Logger {
	return NewWithWriter(service, writerForService(service))
}

// activeLokiSink holds the live sink so the shutdown path can flush it.
// One per process — we only support a single Loki destination.
var activeLokiSink *lokiSink

// StopLoki drains and stops the background Loki pusher, if active. Call
// from main's graceful-shutdown path so the final in-flight batch ships
// before exit. No-op when LOKI_URL was not set.
func StopLoki() {
	if activeLokiSink != nil {
		activeLokiSink.Stop()
	}
}

// writerForService returns os.Stdout, or io.MultiWriter(stdout, lokiSink)
// when LOKI_URL is set. Stdout is kept so `tilt logs` and any local file
// tailing still work — Loki is additive, not a replacement.
func writerForService(service string) io.Writer {
	url := os.Getenv("LOKI_URL")
	if url == "" {
		return os.Stdout
	}
	labels := map[string]string{"service": service}
	if pod := os.Getenv("POD_NAME"); pod != "" {
		labels["pod"] = pod
	}
	activeLokiSink = newLokiSink(url, labels)
	return io.MultiWriter(os.Stdout, activeLokiSink)
}

// NewWithWriter creates a logger that writes to the given writer.
// Useful for testing (write to a buffer instead of stdout).
//
// Request-scoped INFO/DEBUG lines are sampled by trace ID when
// LOG_SAMPLE_RATIO (or OTEL_SAMPLE_RATIO) < 1 — see sampling.go. With the
// env unset every line passes, exactly as before.
func NewWithWriter(service string, w io.Writer) *slog.Logger {
	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: logLevel(),
	})
	return slog.New(newSamplingHandler(handler, requestLogSampleRatio())).With("service", service)
}

// WithTraceID adds a trace_id to the context for propagation.
func WithTraceID(ctx context.Context, traceID string) context.Context {
	return context.WithValue(ctx, traceIDKey, traceID)
}

// TraceIDFromContext extracts the trace_id from context.
// Returns empty string if not set.
func TraceIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(traceIDKey).(string); ok {
		return v
	}
	return ""
}

// WithContext returns a logger with trace_id from context automatically attached.
func WithContext(log *slog.Logger, ctx context.Context) *slog.Logger {
	traceID := TraceIDFromContext(ctx)
	if traceID != "" {
		return log.With("trace_id", traceID)
	}
	return log
}

// logLevel reads LOG_LEVEL env var. Defaults to INFO.
func logLevel() slog.Level {
	switch os.Getenv("LOG_LEVEL") {
	case "debug", "DEBUG":
		return slog.LevelDebug
	case "warn", "WARN":
		return slog.LevelWarn
	case "error", "ERROR":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
