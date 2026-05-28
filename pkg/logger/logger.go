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
func New(service string) *slog.Logger {
	return NewWithWriter(service, os.Stdout)
}

// NewWithWriter creates a logger that writes to the given writer.
// Useful for testing (write to a buffer instead of stdout).
func NewWithWriter(service string, w io.Writer) *slog.Logger {
	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: logLevel(),
	})
	return slog.New(handler).With("service", service)
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
