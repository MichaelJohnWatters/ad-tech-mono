package middleware

import (
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// RateLimiter is a per-client-IP token-bucket limiter for the public,
// browser-facing endpoints (adserver/tracker/ssp/gateway). It sheds abusive
// traffic (scrapers, pixel spam, login brute-force) before it reaches the
// handlers.
//
// Scope caveat (documented, not a bug): the buckets are IN-PROCESS, so the
// effective limit is per-POD — N replicas allow N× the configured rate. That's
// fine as a floor of protection; the real edge shield in prod is Cloudflare (or
// a shared Redis token bucket — a future upgrade if per-pod proves too loose).
// The client IP is read from X-Forwarded-For / X-Real-IP (set by Traefik and
// the CDN), falling back to RemoteAddr; only trust XFF because these services
// sit behind the ingress, never directly on the internet.
type RateLimiter struct {
	rps   rate.Limit
	burst int
	log   *slog.Logger

	mu  sync.Mutex
	ips map[string]*ipEntry
}

type ipEntry struct {
	lim  *rate.Limiter
	seen time.Time
}

// NewRateLimiter returns a limiter allowing rps requests/sec per IP with the
// given burst, or nil when rps <= 0 (disabled) — callers treat a nil limiter as
// "no limiting" via Wrap, which is nil-safe. A background janitor evicts idle
// IPs so the map can't grow unbounded under a churn of source addresses.
func NewRateLimiter(rps, burst int, log *slog.Logger) *RateLimiter {
	if rps <= 0 {
		return nil
	}
	if burst <= 0 {
		burst = rps
	}
	rl := &RateLimiter{
		rps:   rate.Limit(rps),
		burst: burst,
		log:   log,
		ips:   make(map[string]*ipEntry),
	}
	go rl.janitor()
	return rl
}

func (rl *RateLimiter) janitor() {
	for range time.Tick(time.Minute) {
		rl.mu.Lock()
		for ip, e := range rl.ips {
			if time.Since(e.seen) > 10*time.Minute {
				delete(rl.ips, ip)
			}
		}
		rl.mu.Unlock()
	}
}

func (rl *RateLimiter) limiterFor(ip string) *rate.Limiter {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	e, ok := rl.ips[ip]
	if !ok {
		e = &ipEntry{lim: rate.NewLimiter(rl.rps, rl.burst)}
		rl.ips[ip] = e
	}
	e.seen = time.Now()
	return e.lim
}

// Wrap returns next guarded by the limiter. A nil *RateLimiter (disabled) is a
// passthrough, so services can wire it unconditionally. Health/readiness/metrics
// and CORS preflight are never limited (probes and browsers must always pass).
func (rl *RateLimiter) Wrap(next http.Handler) http.Handler {
	if rl == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions || isInfraPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if !rl.limiterFor(clientIP(r)).Allow() {
			w.Header().Set("Retry-After", "1")
			http.Error(w, `{"error":"rate limit exceeded"}`, http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isInfraPath reports whether p is a probe/observability path that must never be
// rate-limited (k8s liveness/readiness, Prometheus scrape, cache-refresh debug).
func isInfraPath(p string) bool {
	return p == "/healthz" || p == "/readyz" || p == "/metrics" ||
		strings.HasPrefix(p, "/debug/")
}

// clientIP extracts the caller's IP, trusting X-Forwarded-For (first hop) then
// X-Real-IP — both set by the Traefik ingress / CDN in front of these services —
// and falling back to the transport RemoteAddr for direct/local calls.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
		return xr
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
