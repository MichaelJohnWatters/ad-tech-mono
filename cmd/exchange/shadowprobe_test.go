package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/optimise"
)

// dspStub answers a no-bid after delay.
func dspStub(t *testing.T, delay time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"nbr":0}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func routerCalls(r *optimise.SmartRouter, ep string) (calls, timeouts int64) {
	for _, s := range r.Stats() {
		if s.DSPID == ep {
			return s.TotalCalls, s.TotalTimeouts
		}
	}
	return 0, 0
}

// TestFanOutShadowProbeDoesNotHoldAuction: a slow ε-probe leg run as a
// shadow (nProbe=1) must not hold the auction — fan-out returns once the
// ranked leg is in — yet its outcome still lands in the router afterwards.
// Red/green against nProbe=0, where the same probe holds the auction.
func TestFanOutShadowProbeDoesNotHoldAuction(t *testing.T) {
	fast := dspStub(t, 5*time.Millisecond)
	slow := dspStub(t, 150*time.Millisecond)
	client := &http.Client{Timeout: time.Second}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	req := openrtb.BidRequest{ID: "t", Imp: []openrtb.Imp{{ID: "1"}}}

	run := func(nProbe int) (time.Duration, *optimise.SmartRouter, *auctionMetrics) {
		router := optimise.NewSmartRouter()
		am := newAuctionMetrics(prometheus.NewRegistry())
		ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
		start := time.Now()
		fanOutToDSPs(ctx, client, []string{fast.URL, slow.URL}, nProbe, req, "display", nil, log, router, nil, "trace-1", false, am)
		took := time.Since(start)
		cancel() // the handler returning must NOT cut the shadow leg short
		return took, router, am
	}

	if took, _, _ := run(0); took < 140*time.Millisecond {
		t.Fatalf("blocking probe: fan-out returned in %v, want it held by the 150ms leg", took)
	}

	took, router, am := run(1)
	if took > 100*time.Millisecond {
		t.Fatalf("shadow probe: fan-out took %v, want it to return once the fast ranked leg answered", took)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		calls, timeouts := routerCalls(router, slow.URL)
		if calls == 1 {
			if timeouts != 0 {
				t.Fatalf("shadow probe recorded as timeout — the request cancel leaked into the probe leg")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("shadow probe outcome never reached the router (calls=%d)", calls)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := testutil.ToFloat64(am.shadowProbes.WithLabelValues(slow.URL, "late")); got != 1 {
		t.Fatalf("shadow_probe_total{late} = %v, want 1", got)
	}
}

// TestFanOutShadowProbeDeadline: a shadow leg still in flight at the
// original fan-out deadline is recorded as a deadline timeout by the
// detached collector, exactly as a blocking leg would have been.
func TestFanOutShadowProbeDeadline(t *testing.T) {
	fast := dspStub(t, 0)
	never := dspStub(t, 2*time.Second)
	client := &http.Client{Timeout: 5 * time.Second}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	router := optimise.NewSmartRouter()
	router.SetKnobs(func() optimise.Knobs { k := optimise.DefaultKnobs(); k.MinCalls = 0; return k })
	// Past the cold-start grace: a deadline only counts for a warm leg.
	router.RecordCall("display", never.URL, false, 0, time.Millisecond, false)
	am := newAuctionMetrics(prometheus.NewRegistry())

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	fanOutToDSPs(ctx, client, []string{fast.URL, never.URL}, 1, openrtb.BidRequest{ID: "t"}, "display", nil, log, router, nil, "trace-2", false, am)
	cancel()

	deadline := time.Now().Add(2 * time.Second)
	for testutil.ToFloat64(am.shadowProbes.WithLabelValues(never.URL, "deadline")) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("in-flight shadow probe never recorded at the fan-out deadline")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, timeouts := routerCalls(router, never.URL); timeouts != 1 {
		t.Fatalf("router timeouts for the stuck probe = %d, want 1", timeouts)
	}
}
