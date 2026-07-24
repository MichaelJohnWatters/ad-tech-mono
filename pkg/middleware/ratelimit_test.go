package middleware

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
}

func do(h http.Handler, path, ip string) int {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Forwarded-For", ip)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

// A burst of requests beyond the bucket gets 429; a different IP is unaffected.
func TestRateLimiterPerIP(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rl := NewRateLimiter(1, 3, log) // 1 rps, burst 3
	h := rl.Wrap(okHandler())

	got200, got429 := 0, 0
	for i := 0; i < 6; i++ {
		if do(h, "/v1/t/imp", "1.2.3.4") == http.StatusOK {
			got200++
		} else {
			got429++
		}
	}
	if got200 != 3 {
		t.Errorf("burst should allow exactly 3, got %d ok", got200)
	}
	if got429 == 0 {
		t.Error("expected some 429s after the burst was exhausted")
	}
	// A different IP has its own bucket — still allowed.
	if code := do(h, "/v1/t/imp", "9.9.9.9"); code != http.StatusOK {
		t.Errorf("second IP should have its own bucket, got %d", code)
	}
}

// Probe/observability paths + preflight are never limited even after exhaustion.
func TestRateLimiterSkipsInfraAndPreflight(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rl := NewRateLimiter(1, 1, log)
	h := rl.Wrap(okHandler())
	// Exhaust the app bucket for this IP.
	do(h, "/v1/t/imp", "5.5.5.5")
	do(h, "/v1/t/imp", "5.5.5.5")
	for _, p := range []string{"/healthz", "/readyz", "/metrics", "/debug/cache/refresh"} {
		if code := do(h, p, "5.5.5.5"); code != http.StatusOK {
			t.Errorf("%s must never be rate-limited, got %d", p, code)
		}
	}
	// OPTIONS preflight always passes.
	req := httptest.NewRequest(http.MethodOptions, "/v1/t/imp", nil)
	req.Header.Set("X-Forwarded-For", "5.5.5.5")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("OPTIONS preflight must pass, got %d", rec.Code)
	}
}

// The live limiter re-reads its rate on every request, so a tune (or an
// enable/disable) takes effect without reconstructing the limiter — this is what
// makes the TierLive <svc>.ratelimit_rps keys actually live.
func TestLiveRateLimiterRereadsRate(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var rps, burst int // start disabled
	rl := NewLiveRateLimiter(func() (int, int) { return rps, burst }, log)
	h := rl.Wrap(okHandler())

	// Disabled (rps=0): everything passes.
	for i := 0; i < 20; i++ {
		if do(h, "/v1/t/imp", "7.7.7.7") != http.StatusOK {
			t.Fatal("rps=0 must pass through")
		}
	}
	// Tune to 1 rps / burst 2 live — no new limiter. Next burst is capped.
	rps, burst = 1, 2
	got200, got429 := 0, 0
	for i := 0; i < 6; i++ {
		if do(h, "/v1/t/imp", "7.7.7.7") == http.StatusOK {
			got200++
		} else {
			got429++
		}
	}
	if got200 != 2 || got429 == 0 {
		t.Errorf("after live tune to burst 2: got %d ok / %d limited, want 2 ok and some 429", got200, got429)
	}
	// Tune back to disabled — passes again immediately.
	rps, burst = 0, 0
	if do(h, "/v1/t/imp", "7.7.7.7") != http.StatusOK {
		t.Error("back to rps=0 must pass through immediately")
	}
}

// A nil (disabled) limiter is a transparent passthrough.
func TestRateLimiterDisabledPassthrough(t *testing.T) {
	var rl *RateLimiter // NewRateLimiter(0,...) returns nil
	h := rl.Wrap(okHandler())
	for i := 0; i < 100; i++ {
		if code := do(h, "/v1/t/imp", "1.1.1.1"); code != http.StatusOK {
			t.Fatalf("disabled limiter must pass everything, got %d", code)
		}
	}
	if NewRateLimiter(0, 5, slog.New(slog.NewTextHandler(io.Discard, nil))) != nil {
		t.Error("rps<=0 should yield a nil (disabled) limiter")
	}
}
