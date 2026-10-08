// shedding.go — the in-flight bid cap (load shedding, handoff 08 lever 3).
//
// A bidder must answer inside the exchange's tmax or say no fast. Queueing
// is the worst outcome: the 2026-10-08 tracker wedge piled 6,090 handlers
// onto one lock, and the stacks+streams+buffers they pinned OOM-killed every
// pod (handoff 08). The root cause is fixed; this rail makes ANY future
// pile-up — whatever causes it — degrade to shed auctions instead of death:
// past the cap, a request gets an instant 204 no-bid (OpenRTB no-bid) and
// the exchange simply counts this DSP out of that auction.
//
// Wraps the shared bid http.HandlerFunc, so it covers both transports (the
// gRPC twin adapts the same handler — see grpc.go). The cap reads live per
// request (dsp.max_inflight_bids): drop it mid-run to watch shedding engage,
// 0 disables. Sheds are visible at adtech_dsp_bids_shed_total.
package main

import (
	"net/http"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
)

func newShedCounter(reg *prometheus.Registry) prometheus.Counter {
	c := prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "adtech",
		Name:      "dsp_bids_shed_total",
		Help:      "Bid requests answered with an instant 204 no-bid because the pod was at dsp.max_inflight_bids. Nonzero = a pile-up was contained.",
	})
	reg.MustRegister(c)
	return c
}

// shedBids caps concurrent executions of next. Over the cap: 204, count,
// return — never queue, never block. The counter admits a tiny transient
// overshoot between Add and the comparison; the rail bounds pile-ups, it
// does not meter exact concurrency.
func shedBids(next http.HandlerFunc, capFn func() int, shed prometheus.Counter) http.HandlerFunc {
	var inflight atomic.Int64
	return func(w http.ResponseWriter, r *http.Request) {
		max := int64(capFn())
		if max <= 0 {
			next(w, r)
			return
		}
		if inflight.Add(1) > max {
			inflight.Add(-1)
			shed.Inc()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		defer inflight.Add(-1)
		next(w, r)
	}
}
