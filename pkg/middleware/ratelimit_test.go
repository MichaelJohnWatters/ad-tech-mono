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
