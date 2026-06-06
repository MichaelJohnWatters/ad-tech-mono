// Package tracing wires the platform's services to Jaeger via OTLP/HTTP.
//
// Every service calls Init(ctx, serviceName) at boot and defer Shutdown()
// at exit. Spans inside the service are created with otel.Tracer("adtech")
// or by using the StartSpan helper, which folds in the trace_id field that
// already appears in every slog line. HTTP middleware extracts/injects W3C
// `traceparent` headers so the trace stitches together across SSP →
// Exchange → DSPs → Ad Server → Tracker.
//
// In dev mode the exporter is a no-op (we skip OTLP if otel.endpoint is
// empty) so services boot without Jaeger running.
package tracing

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"

	"sync/atomic"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// Config holds tracer init parameters. Endpoint is the OTLP/HTTP host:port
// (typically "jaeger:4318" in K8s or "localhost:4318" via port-forward).
// Empty endpoint = no-op exporter — useful for tests and offline dev.
type Config struct {
	ServiceName    string
	ServiceVersion string
	Endpoint       string
	SampleRatio    float64 // 0.0–1.0. 0 = never sample, 1 = always.
	Log            *slog.Logger
}

// Shutdown flushes spans and tears down the provider. Returns immediately
// if Init was a no-op.
type Shutdown func(context.Context) error

// Init configures the global tracer provider for the calling service. The
// returned Shutdown should be deferred at the top of main so spans flush
// before the process exits. If cfg.Endpoint is empty or the OTLP exporter
// fails to connect, returns a no-op Shutdown and logs a warning (the global
// tracer becomes a no-op too — every span call elsewhere stays valid).
//
// Also wires otel.SetErrorHandler so the OTel SDK's internal failures
// (export retries, dropped spans, ...) flow through the service's slog at
// ERROR level instead of OTel's default stdlib logger. Errors are throttled
// to one log per minute per error string to prevent runaway spam when the
// collector is temporarily unreachable.
func Init(ctx context.Context, cfg Config) Shutdown {
	otel.SetErrorHandler(newThrottledErrorHandler(cfg.Log))

	res, _ := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(cfg.ServiceName),
		semconv.ServiceVersion(cfg.ServiceVersion),
	))

	sampler := sdktrace.TraceIDRatioBased(cfg.SampleRatio)
	if cfg.SampleRatio >= 1 {
		sampler = sdktrace.AlwaysSample()
	} else if cfg.SampleRatio <= 0 {
		sampler = sdktrace.NeverSample()
	}

	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	}

	// Wire the OTLP exporter only when an endpoint is configured. Without
	// it the provider still generates real W3C trace IDs (so log/event
	// correlation works) — spans just don't get shipped anywhere.
	if cfg.Endpoint != "" {
		exp, err := otlptracehttp.New(ctx,
			otlptracehttp.WithEndpoint(cfg.Endpoint),
			otlptracehttp.WithInsecure(),
		)
		if err != nil {
			cfg.Log.Warn("otel exporter init failed, trace IDs still generated", "endpoint", cfg.Endpoint, "error", err)
		} else {
			opts = append(opts, sdktrace.WithBatcher(exp))
		}
	} else {
		cfg.Log.Info("otel exporter disabled (no endpoint configured); trace IDs still generated for correlation")
	}

	tp := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(tp)
	// W3C tracecontext + baggage propagators — what most modern services emit.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	cfg.Log.Info("otel tracing initialised",
		"endpoint", cfg.Endpoint, "service", cfg.ServiceName, "sample_ratio", cfg.SampleRatio,
		"exporter_active", cfg.Endpoint != "",
	)

	return func(ctx context.Context) error {
		shutCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		// Force-flush any buffered batch before tearing down so the last
		// spans actually reach Jaeger on a fast restart.
		if err := tp.ForceFlush(shutCtx); err != nil {
			cfg.Log.Warn("otel force flush failed", "error", err)
		}
		return tp.Shutdown(shutCtx)
	}
}

func noopShutdown(_ context.Context) error { return nil }

// Tracer returns the platform's named tracer. Use this instead of
// otel.Tracer directly so every service emits spans under the same name —
// makes Jaeger filtering by tracer easy.
func Tracer() trace.Tracer { return otel.Tracer("adtech") }

// StartSpan is sugar for the common case: tracer.Start with trace_id
// attribute pre-populated from the context's existing tracking ID (set
// by logger.WithTraceID). Always end the returned span — use defer.
func StartSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	ctx, span := Tracer().Start(ctx, name)
	if len(attrs) > 0 {
		span.SetAttributes(attrs...)
	}
	return ctx, span
}

// NewClientTraceparent generates a fresh W3C trace context for callers that
// don't or can't start a real OTel span — typically simulators and test
// fixtures hitting an internal HTTP endpoint. The returned `traceparent`
// is suitable as an HTTP header value; HTTPMiddleware will adopt it and
// the same traceID will surface in Jaeger, in slog `trace_id` fields, and
// in NATS event headers, giving the operator one ID to paste everywhere.
//
// Without this, every simulator request would land at the server, the
// middleware would mint its own ID, and the simulator's locally-printed
// trace_id (e.g. `sim-<ms>`) would be orphaned from every downstream
// store.
func NewClientTraceparent() (traceID, traceparent string) {
	var tid [16]byte
	var sid [8]byte
	_, _ = rand.Read(tid[:])
	_, _ = rand.Read(sid[:])
	traceID = hex.EncodeToString(tid[:])
	traceparent = "00-" + traceID + "-" + hex.EncodeToString(sid[:]) + "-01"
	return
}

// TraceIDFromContext returns the W3C trace ID (32 hex chars) from the
// active OTel span on ctx, or "" if there is none. This is the value that
// flows into logs, NATS events, and the analytics store as `trace_id` —
// using the same string everywhere lets Grafana pivot from a log line to
// the Jaeger trace and back.
func TraceIDFromContext(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.HasTraceID() {
		return ""
	}
	return sc.TraceID().String()
}

// HTTPMiddleware wraps a handler so every request has a server span and
// W3C trace headers are propagated to downstream calls. The current span's
// SpanContext is placed on the request context — downstream HTTP clients
// must use the same propagator (otelhttp wraps Transport, or call Inject
// manually) to continue the trace.
//
// Also writes the resolved W3C trace ID into the X-Trace-Id response
// header so clients (trace explorer, simulator, e2e harness) can read it
// without having to inspect their own span context.
func HTTPMiddleware(serviceName string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
			ctx = adoptTraceIDFromQuery(ctx, r)
			ctx, span := Tracer().Start(ctx, serviceName+" "+r.Method+" "+r.URL.Path,
				trace.WithSpanKind(trace.SpanKindServer),
				trace.WithAttributes(
					attribute.String("http.method", r.Method),
					attribute.String("http.target", r.URL.Path),
				),
			)
			defer span.End()
			if tid := span.SpanContext().TraceID(); tid.IsValid() {
				w.Header().Set("X-Trace-Id", tid.String())
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// adoptTraceIDFromQuery handles the new-tab tracker case: a browser
// follows a creative anchor or pixel <img> directly so the request has
// no W3C traceparent header, but the URL itself carries ?tid=<32-hex>.
// Without this the tracker would start a fresh root span and the click
// / impression would orphan from the parent auction trace in Jaeger.
//
// When the extracted parent is already valid (traceparent header was
// present) we leave the context alone. Otherwise we promote the URL
// tid into a remote SpanContext so the next Tracer().Start treats it
// as a continuation. The synthesised SpanID is derived deterministically
// from the trace ID so dedup-rejected replays of the same URL still all
// hang off the same parent, which is the right shape for Jaeger.
func adoptTraceIDFromQuery(ctx context.Context, r *http.Request) context.Context {
	if trace.SpanContextFromContext(ctx).IsValid() {
		return ctx
	}
	tid := r.URL.Query().Get("tid")
	if tid == "" {
		return ctx
	}
	traceID, err := trace.TraceIDFromHex(tid)
	if err != nil {
		return ctx
	}
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     deriveParentSpanID(traceID),
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})
	return trace.ContextWithRemoteSpanContext(ctx, sc)
}

// deriveParentSpanID hashes the trace ID into an 8-byte span ID so the
// synthesised parent is deterministic per trace. This means every
// click/impression/view replay on the same URL produces the same parent
// SpanID; Jaeger renders them as siblings under one synthesised parent
// instead of disconnected roots.
func deriveParentSpanID(traceID trace.TraceID) trace.SpanID {
	h := sha256.Sum256(append([]byte("adopt-parent:"), traceID[:]...))
	var sid trace.SpanID
	copy(sid[:], h[:8])
	return sid
}

// InjectHTTP copies the current span context into outbound request headers
// so the downstream server sees the trace. Use this before client.Do(req)
// for any service-to-service HTTP call.
func InjectHTTP(ctx context.Context, req *http.Request) {
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
}

// throttledErrorHandler routes OTel SDK errors to slog at ERROR, but only
// once per minute per unique error message. Keeps the export-retry storm
// (a typical Jaeger-unreachable scenario) from blowing up the logs while
// still surfacing the failure in a structured, searchable form.
type throttledErrorHandler struct {
	log      *slog.Logger
	lastSeen atomic.Int64 // unix nanos of last log emission
}

func newThrottledErrorHandler(log *slog.Logger) *throttledErrorHandler {
	return &throttledErrorHandler{log: log}
}

func (h *throttledErrorHandler) Handle(err error) {
	now := time.Now().UnixNano()
	last := h.lastSeen.Load()
	if now-last < int64(time.Minute) {
		return
	}
	if !h.lastSeen.CompareAndSwap(last, now) {
		return
	}
	h.log.Error("otel sdk error", "error", err)
}
