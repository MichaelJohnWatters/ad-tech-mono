package grpcx

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tracing"
)

// mdCarrier adapts gRPC metadata to the OTel TextMapCarrier so the same
// W3C traceparent propagator used for HTTP headers and NATS headers rides
// gRPC metadata. This is the gRPC sibling of tracing.InjectHTTP and
// natsbus's natsHeaderCarrier.
type mdCarrier metadata.MD

func (c mdCarrier) Get(key string) string {
	vs := metadata.MD(c).Get(key)
	if len(vs) == 0 {
		return ""
	}
	return vs[0]
}

func (c mdCarrier) Set(key, value string) { metadata.MD(c).Set(key, value) }

func (c mdCarrier) Keys() []string {
	out := make([]string, 0, len(c))
	for k := range c {
		out = append(out, k)
	}
	return out
}

// unaryClientInterceptor injects the current span context into outgoing
// metadata so the server-side span continues the caller's trace — the
// exact counterpart of tracing.InjectHTTP before client.Do.
func unaryClientInterceptor() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		md, _ := metadata.FromOutgoingContext(ctx)
		md = md.Copy()
		if md == nil {
			md = metadata.MD{}
		}
		otel.GetTextMapPropagator().Inject(ctx, mdCarrier(md))
		return invoker(metadata.NewOutgoingContext(ctx, md), method, req, reply, cc, opts...)
	}
}

// unaryServerInterceptor extracts the caller's trace context from metadata
// and opens a server span, mirroring tracing.HTTPMiddleware so trace_id
// flows identically into logs, NATS events and the analytics store
// regardless of which transport carried the request.
func unaryServerInterceptor(serviceName string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		ctx = otel.GetTextMapPropagator().Extract(ctx, mdCarrier(md))
		ctx, span := tracing.Tracer().Start(ctx, serviceName+" "+info.FullMethod,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(attribute.String("rpc.method", info.FullMethod)),
		)
		defer span.End()
		return handler(ctx, req)
	}
}
