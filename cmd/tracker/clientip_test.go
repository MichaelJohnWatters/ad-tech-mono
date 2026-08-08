package main

import (
	"net/http"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
)

// The parsing tiers (XFF → X-Real-IP → RemoteAddr) are covered in
// pkg/clientip. This pins the tracker wiring: right-anchored XFF (a bot
// prepending forged entries can't dodge the blocklist) with the hop count
// read live from tracker.trusted_proxy_hops.
func TestClientIPFnHonoursTrustedProxyHops(t *testing.T) {
	cfg := config.Load()
	clientIP := newClientIPFn(cfg)

	r := &http.Request{RemoteAddr: "10.0.0.1:5000", Header: http.Header{}}
	r.Header.Set("X-Forwarded-For", "6.6.6.6, 203.0.113.7, 70.41.3.18")

	// Default hops=0: rightmost entry — the client our ingress saw.
	if got := clientIP(r); got != "70.41.3.18" {
		t.Errorf("hops=0: clientIP() = %q, want %q", got, "70.41.3.18")
	}
	// Live-tuned hops=1 (CDN in front of the ingress) shifts one entry left.
	cfg.SetLive("tracker.trusted_proxy_hops", "1")
	if got := clientIP(r); got != "203.0.113.7" {
		t.Errorf("hops=1: clientIP() = %q, want %q", got, "203.0.113.7")
	}
}
