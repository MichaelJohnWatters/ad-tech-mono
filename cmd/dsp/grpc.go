// Internal gRPC twin of POST /v1/openrtb/bid. Only our own exchange dials
// this (grpc://dsp-internal:8182); third-party DSPs are always reached over
// industry-standard OpenRTB JSON/HTTP. The RPC bridges into the exact same
// bid http.HandlerFunc, so both transports run one code path.
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

type internalBidServer struct {
	pb.UnimplementedInternalBidServiceServer
	bid http.HandlerFunc
}

func (s internalBidServer) Bid(ctx context.Context, req *pb.BidRequest) (*pb.BidResponse, error) {
	status, body := grpcx.Bridge(ctx, s.bid, routes.OpenRTBBid, req.GetBody(), req.GetHeaders())
	return &pb.BidResponse{SchemaVersion: 1, Body: body, Status: int32(status)}, nil
}

func startInternalGRPC(lc *lifecycle.Lifecycle, cfg *config.Config, log *slog.Logger, m *middleware.Metrics, bid http.HandlerFunc) {
	grpcx.Start(grpcx.ServerConfig{
		Addr:      ":" + keys.DSP.GRPCPort.Get(cfg),
		Service:   constants.ServiceDSP,
		Log:       log,
		Registry:  m.Registry(),
		Lifecycle: lc,
		Register: func(s *grpc.Server) {
			pb.RegisterInternalBidServiceServer(s, internalBidServer{bid: bid})
		},
	})
}
