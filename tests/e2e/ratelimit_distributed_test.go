//go:build e2e

// Cluster-wide (distributed) rate limiting on the gateway. The per-pod limiter
// (TestRateLimitPerIP) can't block a burst after a fixed count because 3 gateway
// replicas each keep their own bucket — N pods allow N× the rate. With
// gateway.ratelimit_distributed=true the pods share a Redis fixed-window counter,
// so a burst from one IP is capped at the configured rate NO MATTER which pod
// serves each request. This proves that shared-counter behaviour end-to-end: the
// successes over a measured burst stay bounded by the rate (× windows spanned),
// not by rate × replicas — the distinguishing signature of a shared limiter.
package e2e

import (
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestDistributedRateLimitSharedAcrossPods(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	const pod = "gateway-0" // all gateway replicas share this config pod-id
	const rps = 2

	t.Cleanup(func() {
		h.SetConfigForPod(t, "gateway.ratelimit_distributed", "false", pod)
		h.SetConfigForPod(t, "gateway.ratelimit_rps", "0", pod)
		h.SetConfigForPod(t, "gateway.ratelimit_burst", "0", pod)
		h.SetConfigForPod(t, "gateway.ratelimit_allowlist", keys.DefaultRateLimitAllowlist, pod)
	})

	// Empty the allowlist (else our synthetic public IP is exempt), tight limit,
	// and switch on the distributed (Redis) path — all live, no restart.
	h.SetConfigForPod(t, "gateway.ratelimit_allowlist", "", pod)
	h.SetConfigForPod(t, "gateway.ratelimit_burst", "2", pod)
	h.SetConfigForPod(t, "gateway.ratelimit_rps", "2", pod)
	h.SetConfigForPod(t, "gateway.ratelimit_distributed", "true", pod)

	// New connection per request so the L4 LB spreads requests across all 3
	// gateway pods — the whole point of the test.
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	const abusiveIP = "203.0.113.9"
	hit := func() int {
		req, _ := http.NewRequest(http.MethodGet, h.URLs.Gateway+"/sellers.json", nil)
		req.Header.Set("X-Forwarded-For", abusiveIP)
		resp, err := client.Do(req)
		if err != nil {
			return -1
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// Wait for the live config to reach every replica: a burst must start drawing
	// 429s. (Also confirms /sellers.json itself serves 200 — the RLS-hatch path.)
	harness.WaitFor(t, 40*time.Second, "distributed limiter active across gateway pods", func() bool {
		for i := 0; i < 8; i++ {
			if hit() == http.StatusTooManyRequests {
				return true
			}
		}
		return false
	})

	// Measure in a FRESH 1-second window (empty shared counter) so successes
	// reflect the counter's admit rate, not leftover exhaustion. Sleep past a
	// window boundary, then fire a tight burst that stays within ~1 window.
	time.Sleep(1100 * time.Millisecond)
	start := time.Now()
	var got200, got429 int
	for i := 0; i < 15; i++ {
		switch hit() {
		case http.StatusOK:
			got200++
		case http.StatusTooManyRequests:
			got429++
		}
	}
	elapsed := time.Since(start)

	// A shared counter admits exactly `rps` per fresh window; a per-pod limiter
	// across 3 replicas would admit up to rps×3. Bound at rps×2 to tolerate one
	// window boundary — still well below the per-pod ceiling.
	if got200 < 1 {
		t.Errorf("no request succeeded in a fresh window (%d ok / %d limited) — limiter mis-tuned", got200, got429)
	}
	if got200 > rps*2 {
		t.Errorf("got %d successes in a fresh window (burst took %v) — limit is NOT shared across pods; a per-pod limiter would admit up to rps×replicas (=%d)",
			got200, elapsed.Round(time.Millisecond), rps*3)
	}
	if got429 < 8 {
		t.Errorf("distributed limiter under-throttled a 15-burst: %d ok / %d limited — want many 429s", got200, got429)
	}
	t.Logf("fresh-window burst: %d ok / %d limited in %v (shared cap ~%d, per-pod would allow ~%d)",
		got200, got429, elapsed.Round(time.Millisecond), rps, rps*3)
}
