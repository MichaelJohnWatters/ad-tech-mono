package logger

// Request-log sampling: drop per-request INFO/DEBUG lines for trace IDs the
// OTel sampler did not pick, so the requests that keep their log trail are
// EXACTLY the ones that have a Jaeger trace (the Grafana Loki<->Jaeger pivot
// stays coherent for every surviving line). WARN/ERROR always pass, as do
// lines with no trace_id (boot, config, background jobs) — only the
// high-volume request breadcrumbs are sampled.
//
// The decision replicates go.opentelemetry.io/otel/sdk trace.TraceIDRatioBased
// bit-for-bit (bytes 8..16 of the trace ID, big-endian, >>1, compared against
// ratio*2^63) — the two samplers MUST agree or logs and traces would keep
// different 10%s. Ratio comes from LOG_SAMPLE_RATIO, defaulting to
// OTEL_SAMPLE_RATIO so one env var keeps both streams in lockstep; unset (or
// >=1) means log everything, which keeps tests and non-serving services at
// today's behaviour. Flip to 1.0 (redeploy, seconds) when deep-debugging.

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"log/slog"
	"os"
	"strconv"
)

type samplingHandler struct {
	inner slog.Handler
	upper uint64 // sampled iff hash(traceID) < upper
	// boundTraceID remembers a trace_id attached via Logger.With(...) /
	// logger.WithContext — those attrs live in the handler chain, not on
	// each Record, so Handle would never see them otherwise.
	boundTraceID string
}

// newSamplingHandler wraps inner with request-log sampling at ratio.
// ratio >= 1 returns inner unchanged (zero overhead on the default path).
func newSamplingHandler(inner slog.Handler, ratio float64) slog.Handler {
	if ratio >= 1 {
		return inner
	}
	if ratio < 0 {
		ratio = 0
	}
	return &samplingHandler{inner: inner, upper: uint64(ratio * (1 << 63))}
}

func (h *samplingHandler) Enabled(ctx context.Context, lvl slog.Level) bool {
	return h.inner.Enabled(ctx, lvl)
}

func (h *samplingHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn {
		return h.inner.Handle(ctx, r)
	}
	tid := h.boundTraceID
	if tid == "" {
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "trace_id" {
				tid = a.Value.String()
				return false
			}
			return true
		})
	}
	if tid == "" || sampledTraceID(tid, h.upper) {
		return h.inner.Handle(ctx, r)
	}
	return nil // unsampled request breadcrumb — dropped before stdout AND Loki
}

func (h *samplingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	bound := h.boundTraceID
	for _, a := range attrs {
		if a.Key == "trace_id" {
			bound = a.Value.String()
		}
	}
	return &samplingHandler{inner: h.inner.WithAttrs(attrs), upper: h.upper, boundTraceID: bound}
}

func (h *samplingHandler) WithGroup(name string) slog.Handler {
	return &samplingHandler{inner: h.inner.WithGroup(name), upper: h.upper, boundTraceID: h.boundTraceID}
}

// sampledTraceID mirrors traceIDRatioSampler.ShouldSample: uint64 from trace
// ID bytes 8..16 (hex chars 16..32), shifted right once, below upper bound.
// Malformed / non-W3C IDs return true — unknown lines are kept, never lost.
func sampledTraceID(tid string, upper uint64) bool {
	if len(tid) != 32 {
		return true
	}
	b, err := hex.DecodeString(tid[16:])
	if err != nil {
		return true
	}
	return binary.BigEndian.Uint64(b)>>1 < upper
}

// requestLogSampleRatio resolves the sampling ratio at logger construction:
// LOG_SAMPLE_RATIO wins, else OTEL_SAMPLE_RATIO (one knob, both streams),
// else 1.0 (log everything).
func requestLogSampleRatio() float64 {
	s := os.Getenv("LOG_SAMPLE_RATIO")
	if s == "" {
		s = os.Getenv("OTEL_SAMPLE_RATIO")
	}
	if s == "" {
		return 1
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 1
	}
	return v
}
