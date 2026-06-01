//go:build e2e

package harness

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// WaitReady polls /readyz on every service until all return 200 or the
// timeout elapses. Returns a Harness if everything is reachable, fails
// the test with a clear list of unreachable services otherwise.
//
// Typical call site: `h := harness.WaitReady(t, 60*time.Second)`. Use a
// generous timeout — first-time stack bring-up can take 30s+ on Tilt while
// Postgres/NATS/Redis cold-start.
func WaitReady(t *testing.T, timeout time.Duration) *Harness {
	t.Helper()
	h := New(t)

	services := map[string]string{
		"gateway":   h.URLs.Gateway,
		"exchange":  h.URLs.Exchange,
		"dsp":       h.URLs.DSP,
		"dsp-comp1": h.URLs.DSPComp1,
		"dsp-comp2": h.URLs.DSPComp2,
		"tracker":   h.URLs.Tracker,
		"ssp":       h.URLs.SSP,
		"adserver":  h.URLs.AdServer,
		"reporting": h.URLs.Reporting,
	}

	deadline := time.Now().Add(timeout)
	remaining := make(map[string]string, len(services))
	for k, v := range services {
		remaining[k] = v
	}

	for time.Now().Before(deadline) {
		for name, url := range remaining {
			if ready(url + "/readyz") {
				delete(remaining, name)
				t.Logf("service ready: %s", name)
			}
		}
		if len(remaining) == 0 {
			return h
		}
		time.Sleep(500 * time.Millisecond)
	}

	stillDown := make([]string, 0, len(remaining))
	for name := range remaining {
		stillDown = append(stillDown, name)
	}
	t.Fatalf("WaitReady timed out after %s; services not ready: %v\n"+
		"Hint: is `tilt up` running and have all services finished booting?",
		timeout, stillDown)
	return nil
}

func ready(url string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// WaitFor polls fn every 50ms until it returns true or timeout elapses.
// Useful for asserting state that propagates asynchronously, e.g. waiting
// for a warm cache refresh after a NATS invalidate publish.
func WaitFor(t *testing.T, timeout time.Duration, msg string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal(fmt.Sprintf("WaitFor timed out: %s", msg))
}
