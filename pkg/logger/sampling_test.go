package logger

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// Trace IDs chosen so the sampling half (hex chars 16..32) is extreme:
// low hashes far below 0.1*2^63, high hashes far above it.
const (
	sampledTID   = "aaaaaaaaaaaaaaaa0000000000000001" // x=0 -> sampled at any ratio>0
	unsampledTID = "aaaaaaaaaaaaaaaaffffffffffffffff" // x=2^63-1 -> unsampled below 1.0
)

func sampledLogger(t *testing.T, ratio float64) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, nil)
	return slog.New(newSamplingHandler(inner, ratio)), &buf
}

func TestSamplingDropsUnsampledInfo(t *testing.T) {
	log, buf := sampledLogger(t, 0.1)
	log.Info("kept", "trace_id", sampledTID)
	log.Info("dropped", "trace_id", unsampledTID)
	out := buf.String()
	if !strings.Contains(out, "kept") {
		t.Fatalf("sampled trace's INFO line missing: %s", out)
	}
	if strings.Contains(out, "dropped") {
		t.Fatalf("unsampled trace's INFO line should be dropped: %s", out)
	}
}

func TestSamplingKeepsWarnAndErrorAlways(t *testing.T) {
	log, buf := sampledLogger(t, 0.1)
	log.Warn("warn-line", "trace_id", unsampledTID)
	log.Error("error-line", "trace_id", unsampledTID)
	out := buf.String()
	if !strings.Contains(out, "warn-line") || !strings.Contains(out, "error-line") {
		t.Fatalf("WARN/ERROR must always pass: %s", out)
	}
}

func TestSamplingKeepsLinesWithoutTraceID(t *testing.T) {
	log, buf := sampledLogger(t, 0.1)
	log.Info("boot-line", "port", 8080)
	if !strings.Contains(buf.String(), "boot-line") {
		t.Fatalf("non-request line must always pass: %s", buf.String())
	}
}

func TestSamplingHonoursWithBoundTraceID(t *testing.T) {
	log, buf := sampledLogger(t, 0.1)
	log.With("trace_id", unsampledTID).Info("bound-dropped")
	log.With("trace_id", sampledTID).Info("bound-kept")
	out := buf.String()
	if strings.Contains(out, "bound-dropped") {
		t.Fatalf("With-bound unsampled trace should drop INFO: %s", out)
	}
	if !strings.Contains(out, "bound-kept") {
		t.Fatalf("With-bound sampled trace should keep INFO: %s", out)
	}
}

func TestRatioOneIsPassthrough(t *testing.T) {
	log, buf := sampledLogger(t, 1.0)
	log.Info("always", "trace_id", unsampledTID)
	if !strings.Contains(buf.String(), "always") {
		t.Fatalf("ratio 1.0 must log everything: %s", buf.String())
	}
}

func TestMalformedTraceIDIsKept(t *testing.T) {
	log, buf := sampledLogger(t, 0.1)
	log.Info("odd-id", "trace_id", "not-a-w3c-id")
	if !strings.Contains(buf.String(), "odd-id") {
		t.Fatalf("malformed trace IDs must be kept, never lost: %s", buf.String())
	}
}
