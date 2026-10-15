package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// Concurrent legs to one DSP must REUSE connections: with the default
// transport (2 idle/host) a burst of 50 concurrent requests repeated 4×
// re-dials most legs every round; the fan-out pool keeps them warm, so dials
// stay at ~the peak concurrency, not ~the request count.
func TestFanoutTransport_ReusesConnections(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"nbr":0}`)
	}))
	defer srv.Close()

	dials := prometheus.NewCounter(prometheus.CounterOpts{Name: "t_dials"})
	c := &http.Client{Transport: fanoutTransport(dials)}
	const conc, rounds = 50, 4
	for r := 0; r < rounds; r++ {
		var wg sync.WaitGroup
		for i := 0; i < conc; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				resp, err := c.Get(srv.URL)
				if err != nil {
					t.Error(err)
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}()
		}
		wg.Wait()
	}
	if got := testutil.ToFloat64(dials); got > conc {
		t.Fatalf("dials=%v for %d requests at concurrency %d — connections not reused", got, conc*rounds, conc)
	}
}
