package grpcx

import (
	"context"
	"net/http"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/MichaelJohnWatters/ad-tech-mono/pkg/proto/internalrpc"
)

// Package-level connection pool, one multiplexed HTTP/2 conn per target —
// the gRPC sibling of the http.DefaultClient the internal HTTP paths use.
// Conns are lazy (grpc.NewClient doesn't dial until first RPC) and live
// for the process lifetime; gRPC reconnects transparently underneath.
var (
	poolMu sync.Mutex
	pool   = map[string]*grpc.ClientConn{}
)

func conn(target string) (*grpc.ClientConn, error) {
	poolMu.Lock()
	defer poolMu.Unlock()
	if c, ok := pool[target]; ok {
		return c, nil
	}
	// dns:/// + round_robin: against a headless k8s Service the resolver
	// sees every pod IP and spreads RPCs across replicas. gRPC re-resolves
	// on connection loss, so pod restarts rebalance; a plain ClusterIP
	// target would pin the single HTTP/2 conn to one pod.
	c, err := grpc.NewClient("dns:///"+target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(unaryClientInterceptor()),
		grpc.WithDefaultServiceConfig(`{"loadBalancingConfig":[{"round_robin":{}}]}`),
	)
	if err != nil {
		return nil, err
	}
	pool[target] = c
	return c, nil
}

// RunAuction calls the exchange's gRPC twin of POST /v1/openrtb/auction.
// body is the OpenRTB BidRequest JSON; returns the HTTP-equivalent status
// and the OpenRTB BidResponse JSON.
func RunAuction(ctx context.Context, target string, body []byte, headers map[string]string) (int, []byte, error) {
	c, err := conn(target)
	if err != nil {
		return 0, nil, err
	}
	resp, err := pb.NewInternalAuctionServiceClient(c).RunAuction(ctx,
		&pb.RunAuctionRequest{SchemaVersion: 1, Body: body, Headers: headers})
	if err != nil {
		return 0, nil, err
	}
	return respStatus(resp.GetStatus()), resp.GetBody(), nil
}

// Bid calls our DSP's gRPC twin of POST /v1/openrtb/bid. body is the
// OpenRTB BidRequest JSON; returns the HTTP-equivalent status and the
// OpenRTB BidResponse JSON.
func Bid(ctx context.Context, target string, body []byte, headers map[string]string) (int, []byte, error) {
	c, err := conn(target)
	if err != nil {
		return 0, nil, err
	}
	resp, err := pb.NewInternalBidServiceClient(c).Bid(ctx,
		&pb.BidRequest{SchemaVersion: 1, Body: body, Headers: headers})
	if err != nil {
		return 0, nil, err
	}
	return respStatus(resp.GetStatus()), resp.GetBody(), nil
}

// ServeAd calls the ad server's gRPC twin of POST /v1/ad/serve. body is
// the models.ServeRequest JSON; returns the HTTP-equivalent status (429 =
// frequency-cap decline) and the models.ServeResponse JSON.
func ServeAd(ctx context.Context, target string, body []byte, headers map[string]string) (int, []byte, error) {
	c, err := conn(target)
	if err != nil {
		return 0, nil, err
	}
	resp, err := pb.NewInternalAdServeServiceClient(c).Serve(ctx,
		&pb.ServeRequest{SchemaVersion: 1, Body: body, Headers: headers})
	if err != nil {
		return 0, nil, err
	}
	return respStatus(resp.GetStatus()), resp.GetBody(), nil
}

// respStatus normalises an unset envelope status to 200 OK.
func respStatus(s int32) int {
	if s == 0 {
		return http.StatusOK
	}
	return int(s)
}
