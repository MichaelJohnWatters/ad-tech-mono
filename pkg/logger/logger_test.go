package logger_test

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
)

func TestNewLogger(t *testing.T) {
	var buf bytes.Buffer
	log := logger.NewWithWriter("dsp", &buf)

	log.Info("test message", "key", "value")

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("failed to parse log output: %v", err)
	}

	if entry["service"] != "dsp" {
		t.Errorf("service = %v, want dsp", entry["service"])
	}
	if entry["msg"] != "test message" {
		t.Errorf("msg = %v, want 'test message'", entry["msg"])
	}
	if entry["key"] != "value" {
		t.Errorf("key = %v, want 'value'", entry["key"])
	}
	if entry["level"] != "INFO" {
		t.Errorf("level = %v, want INFO", entry["level"])
	}
}

func TestWithContext_TraceID(t *testing.T) {
	var buf bytes.Buffer
	log := logger.NewWithWriter("tracker", &buf)

	ctx := logger.WithTraceID(context.Background(), "trace-abc-123")
	ctxLog := logger.WithContext(log, ctx)

	ctxLog.Info("impression received")

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("failed to parse log output: %v", err)
	}

	if entry["trace_id"] != "trace-abc-123" {
		t.Errorf("trace_id = %v, want trace-abc-123", entry["trace_id"])
	}
	if entry["service"] != "tracker" {
		t.Errorf("service = %v, want tracker", entry["service"])
	}
}

func TestTraceIDFromContext_Empty(t *testing.T) {
	ctx := context.Background()
	if got := logger.TraceIDFromContext(ctx); got != "" {
		t.Errorf("expected empty trace_id, got %q", got)
	}
}

func TestWithContext_NoTraceID(t *testing.T) {
	var buf bytes.Buffer
	log := logger.NewWithWriter("gateway", &buf)

	ctx := context.Background()
	ctxLog := logger.WithContext(log, ctx)

	ctxLog.Info("no trace")

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("failed to parse log output: %v", err)
	}

	if _, exists := entry["trace_id"]; exists {
		t.Error("trace_id should not be present when not set in context")
	}
}
