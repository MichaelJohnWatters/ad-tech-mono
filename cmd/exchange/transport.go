package main

import (
	"context"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Fan-out connection pool. The exchange calls every external DSP over
// OpenRTB HTTP on each auction; with Go's DefaultTransport only 2 idle
// connections per host are kept, so at load most legs found no idle conn
// and DIALED a fresh TCP connection — ~12% of exchange CPU in
// net/http.(*Transport).dialConnFor at 250rps (pprof 2026-10-15), on top of
// syscalls already being its biggest cost. Concurrent legs per DSP ≈ rps ×
// leg latency (250 × ~0.7s ≈ 175 under saturation), so the per-host idle
// pool must be at least that to keep connections warm.
const (
	fanoutMaxIdlePerHost = 256
	fanoutMaxIdle        = 1024
	fanoutIdleTimeout    = 90 * time.Second
)

// auctionDialCounter counts NEW outbound TCP connections from the fan-out
// client — the direct read on pool health (a warm pool dials ~0/s at steady
// state; a cold or undersized one dials per leg). Registered in main.
var auctionDialCounter = prometheus.NewCounter(prometheus.CounterOpts{
	Namespace: "adtech",
	Name:      "exchange_fanout_dials_total",
	Help:      "New TCP connections dialed by the exchange's DSP fan-out HTTP client. Should be ~flat at steady state; a rising rate means connections aren't being reused.",
})

// fanoutTransport clones DefaultTransport (keeps its dial/TLS timeouts,
// proxy and HTTP/2 settings) with a fan-out-sized idle pool, and counts
// dials. A leg cancelled at the fan-out deadline still closes its conn —
// that's inherent; the pool covers the healthy majority.
func fanoutTransport(dials prometheus.Counter) *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = fanoutMaxIdle
	t.MaxIdleConnsPerHost = fanoutMaxIdlePerHost
	t.IdleConnTimeout = fanoutIdleTimeout
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if dials != nil {
			dials.Inc()
		}
		return dialer.DialContext(ctx, network, addr)
	}
	return t
}
