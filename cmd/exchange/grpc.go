// Internal gRPC twin of POST /v1/openrtb/auction. Only our own SSP dials
// this (grpc://exchange:8181); anything external — Prebid, third-party
// SSPs — keeps the OpenRTB HTTP endpoint. The RPC bridges into the exact
// same auction http.HandlerFunc, so both transports run one code path.
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

type internalAuctionServer struct {
	pb.UnimplementedInternalAuctionServiceServer
	auction http.HandlerFunc
}

func (s internalAuctionServer) RunAuction(ctx context.Context, req *pb.RunAuctionRequest) (*pb.RunAuctionResponse, error) {
	status, body := grpcx.Bridge(ctx, s.auction, routes.OpenRTBAuction, req.GetBody(), req.GetHeaders())
	return &pb.RunAuctionResponse{SchemaVersion: 1, Body: body, Status: int32(status)}, nil
}

func startInternalGRPC(lc *lifecycle.Lifecycle, cfg *config.Config, log *slog.Logger, m *middleware.Metrics, auction http.HandlerFunc) {
	grpcx.Start(grpcx.ServerConfig{
		Addr:      ":" + keys.Exchange.GRPCPort.Get(cfg),
		Service:   constants.ServiceExchange,
		Log:       log,
		Registry:  m.Registry(),
		Lifecycle: lc,
		Register: func(s *grpc.Server) {
			pb.RegisterInternalAuctionServiceServer(s, internalAuctionServer{auction: auction})
		},
	})
}
