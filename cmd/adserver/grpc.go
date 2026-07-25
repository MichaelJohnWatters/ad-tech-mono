// Internal gRPC twin of POST /v1/ad/serve. Only our own SSP dials this
// (grpc://adserver:8185); everything browser-facing stays HTTP. The RPC
// bridges into the exact same serve http.HandlerFunc, so both transports
// run one code path — including the 429 frequency-cap decline, which
// rides the envelope's HTTP-equivalent status.
package main

import (
	"context"
	"log/slog"
	"net/http"

	"google.golang.org/grpc"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/grpcx"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	pb "github.com/MichaelJohnWatters/ad-tech-mono/pkg/proto/internalrpc"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

type internalAdServeServer struct {
	pb.UnimplementedInternalAdServeServiceServer
	serve http.HandlerFunc
}

func (s internalAdServeServer) Serve(ctx context.Context, req *pb.ServeRequest) (*pb.ServeResponse, error) {
	status, body := grpcx.Bridge(ctx, s.serve, routes.AdServe, req.GetBody(), req.GetHeaders())
	return &pb.ServeResponse{SchemaVersion: 1, Body: body, Status: int32(status)}, nil
}

func startInternalGRPC(lc *lifecycle.Lifecycle, cfg *config.Config, log *slog.Logger, m *middleware.Metrics, serve http.HandlerFunc) {
	grpcx.Start(grpcx.ServerConfig{
		Addr:      ":" + keys.AdServer.GRPCPort.Get(cfg),
		Service:   constants.ServiceAdServer,
		Log:       log,
		Registry:  m.Registry(),
		Lifecycle: lc,
		Register: func(s *grpc.Server) {
			pb.RegisterInternalAdServeServiceServer(s, internalAdServeServer{serve: serve})
		},
	})
}
