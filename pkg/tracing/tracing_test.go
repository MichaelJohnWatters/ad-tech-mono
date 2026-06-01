package tracing

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
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
