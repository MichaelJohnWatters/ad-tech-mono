package grpcx

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
)

// ServerConfig wires an internal gRPC listener into a service.
type ServerConfig struct {
	// Addr is the listen address, e.g. ":8181".
	Addr string
	// Service is the platform service name (constants.Service*) used for
	// span names and metric labels.
	Service string
	Log     *slog.Logger
	// Registry, when set, receives adtech_grpc_* request metrics — pass
	// middleware.NewMetrics(service).Registry() so gRPC and HTTP metrics
	// share one /metrics scrape.
	Registry prometheus.Registerer
	// Lifecycle registers graceful drain alongside the HTTP server.
	Lifecycle *lifecycle.Lifecycle
	// Register attaches the service implementations to the server.
	Register func(*grpc.Server)
	// Listener, when set, is served directly instead of binding Addr —
	// lets tests use an ephemeral port they already know.
	Listener net.Listener
}

// Start binds and serves an internal gRPC listener in the background and
// registers graceful drain on the lifecycle. It is the gRPC sibling of
// lifecycle.ServeHTTP, but non-blocking: call it before the final blocking
// ServeHTTP in main. Bind failures retry like the HTTP path and surface
// as ERROR logs if they never succeed.
func Start(cfg ServerConfig) {
	interceptors := []grpc.UnaryServerInterceptor{unaryServerInterceptor(cfg.Service)}
	if cfg.Registry != nil {
		interceptors = append(interceptors, metricsInterceptor(cfg.Service, cfg.Registry))
	}
	srv := grpc.NewServer(grpc.ChainUnaryInterceptor(interceptors...))
	cfg.Register(srv)

	// FIRST: in-flight RPCs drain before dependency teardown, same
	// ordering contract as the HTTP server.
	cfg.Lifecycle.OnShutdownFirst("grpc-server", func(ctx context.Context) error {
		done := make(chan struct{})
		go func() {
			srv.GracefulStop()
			close(done)
		}()
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			srv.Stop()
			return fmt.Errorf("grpc graceful stop timed out, forced: %w", ctx.Err())
		}
	})

	go func() {
		if cfg.Listener != nil {
			if err := srv.Serve(cfg.Listener); err != nil {
				cfg.Log.Error("grpc server failed", "addr", cfg.Listener.Addr().String(), "error", err)
			}
			return
		}
		const maxRetries = 30
		var lastErr error
		for i := 0; i <= maxRetries; i++ {
			lis, err := net.Listen("tcp", cfg.Addr)
			if err != nil {
				lastErr = err
				cfg.Log.Warn("grpc port in use, retrying", "addr", cfg.Addr, "attempt", i+1)
				time.Sleep(1 * time.Second)
				continue
			}
			cfg.Log.Info("grpc server listening", "addr", cfg.Addr)
			if err := srv.Serve(lis); err != nil {
				cfg.Log.Error("grpc server failed", "addr", cfg.Addr, "error", err)
			}
			return
		}
		cfg.Log.Error("grpc server failed to bind", "addr", cfg.Addr, "error", lastErr)
	}()
}

// metricsInterceptor records per-RPC count + latency into the service's
// prometheus registry, labelled like the HTTP middleware so dashboards
// can union both transports. status is the HTTP-equivalent code carried
// in the envelope response (all internalrpc responses expose GetStatus),
// falling back to the gRPC code for transport-level errors.
func metricsInterceptor(serviceName string, reg prometheus.Registerer) grpc.UnaryServerInterceptor {
	requests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "adtech",
		Subsystem: "grpc",
		Name:      "requests_total",
		Help:      "Total gRPC requests by method + HTTP-equivalent status.",
	}, []string{"service", "handler", "status"})
	duration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "adtech",
		Subsystem: "grpc",
		Name:      "request_duration_seconds",
		Help:      "gRPC request latency by method + HTTP-equivalent status.",
		Buckets: []float64{
			0.001, 0.002, 0.005, 0.01, 0.02, 0.05,
			0.1, 0.2, 0.5, 1, 2, 5,
		},
	}, []string{"service", "handler", "status"})
	reg.MustRegister(requests, duration)

	type statuser interface{ GetStatus() int32 }

	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		code := ""
		switch {
		case err != nil:
			code = "grpc-" + status.Code(err).String()
		default:
			if s, ok := resp.(statuser); ok {
				code = strconv.Itoa(int(s.GetStatus()))
			} else {
				code = "200"
			}
		}
		labels := prometheus.Labels{"service": serviceName, "handler": info.FullMethod, "status": code}
		requests.With(labels).Inc()
		duration.With(labels).Observe(time.Since(start).Seconds())
		return resp, err
	}
}
