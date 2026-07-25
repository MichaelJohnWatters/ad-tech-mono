package grpcx

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
	pb "github.com/MichaelJohnWatters/ad-tech-mono/pkg/proto/internalrpc"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tracing"
)

func nopLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestURLHelpers(t *testing.T) {
	if !IsURL("grpc://dsp-internal:8182") {
		t.Fatal("grpc:// URL not detected")
	}
	if IsURL("http://dsp-internal:8082") {
		t.Fatal("http URL misdetected as grpc")
	}
	if got := Target(" grpc://exchange:8181"); got != "exchange:8181" {
		t.Fatalf("Target = %q", got)
	}
}

// TestBridgeRoundtrip proves the full loop: a bridged http.HandlerFunc
// served over a real gRPC listener, called through the typed client
// helpers, preserving body, control headers, non-200 status, and the
// caller's W3C trace context.
func TestBridgeRoundtrip(t *testing.T) {
	otel.SetTracerProvider(sdktrace.NewTracerProvider())
	otel.SetTextMapPropagator(propagation.TraceContext{})

	var gotTraceID, gotHeader string
	handler := func(w http.ResponseWriter, r *http.Request) {
		gotTraceID = tracing.TraceIDFromContext(r.Context())
		gotHeader = r.Header.Get("X-Dev-Delay-Ms")
		var in map[string]string
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		if in["decline"] == "freqcap" {
			http.Error(w, "frequency cap exceeded", http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"echo": in["msg"]})
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	lc := lifecycle.New(nopLogger())
	Start(ServerConfig{
		Service:   "test",
		Log:       nopLogger(),
		Lifecycle: lc,
		Listener:  lis,
		Register: func(s *grpc.Server) {
			pb.RegisterInternalBidServiceServer(s, bidBridge{h: handler})
		},
	})
	defer func() { _ = lc.Shutdown(2 * time.Second) }()

	ctx, span := tracing.Tracer().Start(context.Background(), "test-client")
	defer span.End()
	wantTraceID := span.SpanContext().TraceID().String()

	target := lis.Addr().String()
	body, _ := json.Marshal(map[string]string{"msg": "hello"})
	status, respBody, err := Bid(ctx, target, body, map[string]string{"X-Dev-Delay-Ms": "150"})
	if err != nil {
		t.Fatalf("Bid: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	var out map[string]string
	if err := json.Unmarshal(respBody, &out); err != nil {
		t.Fatalf("response body not JSON: %v (%q)", err, respBody)
	}
	if out["echo"] != "hello" {
		t.Fatalf("echo = %q", out["echo"])
	}
	if gotHeader != "150" {
		t.Fatalf("forwarded header = %q, want 150", gotHeader)
	}
	if gotTraceID != wantTraceID {
		t.Fatalf("trace ID did not propagate: handler saw %q, client sent %q", gotTraceID, wantTraceID)
	}

	// Non-200 statuses must survive the transport (429 = freq-cap no-fill).
	body, _ = json.Marshal(map[string]string{"decline": "freqcap"})
	status, _, err = Bid(ctx, target, body, nil)
	if err != nil {
		t.Fatalf("Bid decline: %v", err)
	}
	if status != http.StatusTooManyRequests {
		t.Fatalf("decline status = %d, want 429", status)
	}
}

// bidBridge is the same 5-line adapter each service writes to expose an
// existing handler over the internal gRPC transport.
type bidBridge struct {
	pb.UnimplementedInternalBidServiceServer
	h http.HandlerFunc
}

func (b bidBridge) Bid(ctx context.Context, req *pb.BidRequest) (*pb.BidResponse, error) {
	status, body := Bridge(ctx, b.h, "/v1/openrtb/bid", req.GetBody(), req.GetHeaders())
	return &pb.BidResponse{SchemaVersion: 1, Body: body, Status: int32(status)}, nil
}
