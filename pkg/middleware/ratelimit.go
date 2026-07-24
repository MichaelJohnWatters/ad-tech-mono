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
	// cfgFn is read on EVERY request so the config is genuinely live — the
	// <svc>.ratelimit_* keys are TierLive, and an operator (or a test) can tune
	// them without restarting the pod. Returns (rps, burst, trustedHops); rps <= 0
	// disables limiting for that request.
	cfgFn func() (int, int, int)
	log   *slog.Logger

	mu       sync.Mutex
	ips      map[string]*ipEntry
	curRPS   int // the rps/burst the current buckets were built with; a change
	curBurst int // rebuilds them so a live tune takes effect immediately.
}

type ipEntry struct {
	lim  *rate.Limiter
	seen time.Time
}

// NewLiveRateLimiter returns a limiter whose rate is resolved from cfgFn on
// every request, so <svc>.ratelimit_rps can be tuned live (it's TierLive). rps
// <= 0 means "disabled" for that window — the limiter passes requests through.
// Never returns nil; the passthrough is decided per request. A background
// janitor evicts idle IPs so the map can't grow unbounded.
func NewLiveRateLimiter(cfgFn func() (rps, burst, trustedHops int), log *slog.Logger) *RateLimiter {
	rl := &RateLimiter{cfgFn: cfgFn, log: log, ips: make(map[string]*ipEntry)}
	go rl.janitor()
	return rl
}

// NewRateLimiter is the fixed-rate constructor (trustedHops=0 → rightmost XFF).
// Returns nil when rps <= 0 (disabled) — callers treat a nil limiter as "no
// limiting" via the nil-safe Wrap. Retained for callers/tests that want a
// constant rate; services use NewLiveRateLimiter so the TierLive keys take
// effect live.
func NewRateLimiter(rps, burst int, log *slog.Logger) *RateLimiter {
	if rps <= 0 {
		return nil
	}
	return NewLiveRateLimiter(func() (int, int, int) { return rps, burst, 0 }, log)
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

func (rl *RateLimiter) limiterFor(ip string, rps, burst int) *rate.Limiter {
	if burst <= 0 {
		burst = rps
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	// A live rate change rebuilds every bucket so the new limit takes effect at
	// once (an existing token bucket keeps its original rate otherwise).
	if rps != rl.curRPS || burst != rl.curBurst {
		rl.curRPS, rl.curBurst = rps, burst
		rl.ips = make(map[string]*ipEntry)
	}
	e, ok := rl.ips[ip]
	if !ok {
		e = &ipEntry{lim: rate.NewLimiter(rate.Limit(rps), burst)}
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
		rps, burst, trustedHops := rl.cfgFn()
		if rps <= 0 { // disabled live — pass through
			next.ServeHTTP(w, r)
			return
		}
		if !rl.limiterFor(clientIP(r, trustedHops), rps, burst).Allow() {
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

// clientIP extracts the caller's IP for rate-limit bucketing. trustedHops is the
// number of trusted reverse proxies in front of the app; the real client is that
// many entries from the RIGHT of X-Forwarded-For, because each proxy APPENDS the
// address it received the connection from. Taking a right-anchored entry is what
// makes the limit spoof-resistant: a client can PREPEND fake X-Forwarded-For
// values, but it can't forge the entries a trusted proxy appended after them —
// so it can't mint a fresh bucket per request by rotating the header.
//
//   - trustedHops=0 (default): the app's direct upstream (our ingress) is the
//     only trusted hop → use the rightmost XFF entry (the client the ingress saw).
//   - trustedHops=1: a CDN sits in front of the ingress → skip the ingress entry,
//     use the next one (the client the CDN saw). And so on.
//
// Falls back to X-Real-IP, then RemoteAddr, when XFF is absent (direct/local).
func clientIP(r *http.Request, trustedHops int) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		idx := len(parts) - 1 - trustedHops
		if idx < 0 {
			idx = 0 // more hops claimed than present → the leftmost is the best we have
		}
		if ip := strings.TrimSpace(parts[idx]); ip != "" {
			return ip
		}
	}
	if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
		return xr
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
