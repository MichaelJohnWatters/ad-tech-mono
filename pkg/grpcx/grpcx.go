// Package grpcx is the platform's internal gRPC transport layer.
//
// It exists for exactly one class of edge: hot-path calls where this
// platform owns BOTH ends (SSP -> exchange, exchange -> our own DSP,
// SSP -> ad server). Every boundary an external party can sit on stays
// industry-standard OpenRTB JSON/HTTP — the exchange's fan-out to
// third-party DSPs, win/loss notices, browser-facing serving/trackers.
//
// Which transport an edge uses is decided per endpoint by URL scheme:
// grpc://host:port selects gRPC, anything else stays HTTP. That makes
// rollback (and A/B) a config change, not a deploy, and lets one
// dsp_endpoints list mix our gRPC DSP with HTTP third parties.
//
// Servers bridge RPCs into the exact same http.HandlerFunc that serves
// the HTTP twin endpoint (see Bridge), so both transports execute one
// code path and cannot drift. Trace context propagates through gRPC
// metadata with the same OTel propagator the HTTP and NATS paths use.
package grpcx

import "strings"

// scheme prefixes an endpoint URL to select the gRPC transport.
const scheme = "grpc://"

// IsURL reports whether the endpoint URL selects the gRPC transport.
func IsURL(u string) bool {
	return strings.HasPrefix(strings.TrimSpace(u), scheme)
}

// Target strips the grpc:// scheme, returning the host:port dial target.
func Target(u string) string {
	return strings.TrimPrefix(strings.TrimSpace(u), scheme)
}
