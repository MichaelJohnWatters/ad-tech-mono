package tracing

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

func nopLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(nopWriter{}, nil))
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestInit_EmptyEndpointReturnsNoop(t *testing.T) {
	sd := Init(context.Background(), Config{ServiceName: "x", Log: nopLogger()})
	if err := sd(context.Background()); err != nil {
		t.Errorf("noop shutdown should not error, got %v", err)
	}
}

func TestStartSpan_NoopProviderStillReturnsValidSpan(t *testing.T) {
	ctx, span := StartSpan(context.Background(), "test.span")
	if span == nil {
		t.Fatal("StartSpan returned nil span")
	}
	span.End()
	if ctx == nil {
		t.Error("StartSpan returned nil context")
	}
}

func TestHTTPMiddleware_PropagatesTraceContext(t *testing.T) {
	// Inject a traceparent header on the inbound request; the middleware must
	// extract it and place a span on the request context. Downstream handler
	// then has access to the propagated context.
	mw := HTTPMiddleware("test")
	var sawTraceparent bool

	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		hdr := r.Header.Get("traceparent")
		sawTraceparent = hdr != ""
	})

	prop := propagation.TraceContext{}
	otel.SetTextMapPropagator(prop)

	srv := httptest.NewServer(mw(inner))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/x", nil)
	req.Header.Set("traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if !sawTraceparent {
		t.Error("traceparent header was not propagated to downstream handler")
	}
}

// Browser following the tracker click anchor in a new tab drops the
// traceparent header, but the URL itself carries ?tid=<32-hex>. The
// middleware must adopt that as the trace ID so the click span shows
// up under the same trace as the impression/view in Jaeger.
func TestHTTPMiddleware_AdoptsTraceIDFromQueryParam(t *testing.T) {
	const tid = "0af7651916cd43dd8448eb211c80319c"
	mw := HTTPMiddleware("test")
	var serverSpanTraceID string

	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		serverSpanTraceID = trace.SpanFromContext(r.Context()).SpanContext().TraceID().String()
	})
	otel.SetTextMapPropagator(propagation.TraceContext{})
	srv := httptest.NewServer(mw(inner))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/t/click?tid=" + tid)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if serverSpanTraceID != tid {
		t.Errorf("expected server span trace ID = %s, got %s", tid, serverSpanTraceID)
	}
}

// When the query carries a malformed tid (not 32 hex) the middleware
// must NOT adopt it — better to start a fresh root span than to corrupt
// the trace ID space with junk inputs.
func TestHTTPMiddleware_RejectsMalformedTraceIDQueryParam(t *testing.T) {
	mw := HTTPMiddleware("test")
	var serverSpanTraceID string

	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		serverSpanTraceID = trace.SpanFromContext(r.Context()).SpanContext().TraceID().String()
	})
	otel.SetTextMapPropagator(propagation.TraceContext{})
	srv := httptest.NewServer(mw(inner))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/t/click?tid=not-a-trace-id")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if serverSpanTraceID == "not-a-trace-id" {
		t.Errorf("malformed tid must not be adopted as trace ID")
	}
}
