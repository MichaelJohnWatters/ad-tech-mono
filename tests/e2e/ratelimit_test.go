//go:build e2e

// Per-IP rate-limit enforcement (pkg/middleware.RateLimiter on the gateway).
package e2e

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// TestRateLimitPerIP proves the gateway's per-IP rate limiter both BLOCKS an
// abusive burst (429) and stays scoped per source IP + recovers — the live
// enforcement counterpart to the pkg/middleware unit test.
//
// It tunes gateway.ratelimit_rps live (the key is TierLive and the limiter now
// re-reads it per request, so no pod restart is needed) to 1 rps / burst 1, then
// hammers a public endpoint (/sellers.json, 200, no auth) from one X-Forwarded-For
// IP. Buckets are per-pod (3 gateway replicas), so a burst can't be blocked
// after a fixed count — instead we assert the strong invariants: an abusive IP
// eventually gets 429s, a DIFFERENT IP still gets through, and the first IP
// recovers once its bucket refills.
func TestRateLimitPerIP(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	const pod = "gateway-0"
	t.Cleanup(func() {
		h.SetConfigForPod(t, "gateway.ratelimit_rps", "0", pod)
		h.SetConfigForPod(t, "gateway.ratelimit_burst", "0", pod)
		h.SetConfigForPod(t, "gateway.ratelimit_allowlist", keys.DefaultRateLimitAllowlist, pod)
	})

	// EMPTY the allowlist for the test so our synthetic client IPs aren't
	// exempted (the shipped default allowlists all private ranges) — then we can
	// hammer from one IP and actually get blocked.
	h.SetConfigForPod(t, "gateway.ratelimit_allowlist", "", pod)
	// Tight limit, live — 1 request/sec, burst 1, per pod.
	h.SetConfigForPod(t, "gateway.ratelimit_burst", "1", pod)
	h.SetConfigForPod(t, "gateway.ratelimit_rps", "1", pod)

	const abusiveIP = "203.0.113.7"
	hit := func(ip string) int {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.URLs.Gateway+"/sellers.json", nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		req.Header.Set("X-Forwarded-For", ip)
		resp, err := h.HTTP.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// 1) REJECT: a burst from one IP must eventually draw 429s. Retried so the
	// live-config change has time to reach all gateway replicas.
	harness.WaitFor(t, 40*time.Second, "an abusive burst from one IP gets 429", func() bool {
		got429 := false
		for i := 0; i < 40; i++ {
			if hit(abusiveIP) == http.StatusTooManyRequests {
				got429 = true
			}
		}
		return got429
	})

	// 2) ISOLATION: a different source IP has its own bucket — the first IP's
	// exhaustion must not starve it.
	freshOK := false
	for i := 0; i < 10 && !freshOK; i++ {
		if hit("198.51.100.42") == http.StatusOK {
			freshOK = true
		}
	}
	if !freshOK {
		t.Error("a different IP should have its own bucket and still get 200")
	}

	// 3) RECOVERY: once the token bucket refills (1/sec), the abusive IP is
	// served again — the limit throttles, it doesn't permanently ban.
	harness.WaitFor(t, 15*time.Second, "the throttled IP recovers after the bucket refills", func() bool {
		return hit(abusiveIP) == http.StatusOK
	})
}
