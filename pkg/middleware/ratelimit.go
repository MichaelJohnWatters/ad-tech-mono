package middleware

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clientip"
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
// RateLimitConfig is the live per-request configuration the limiter reads from
// cfgFn. Every field is resolved fresh on each request so the TierLive
// <svc>.ratelimit_* keys take effect without a restart.
type RateLimitConfig struct {
	RPS         int    // sustained requests/sec per client IP; <= 0 disables limiting
	Burst       int    // token-bucket burst; <= 0 defaults to RPS
	TrustedHops int    // X-Forwarded-For entries-from-the-right added by trusted proxies
	Allowlist   string // comma-separated CIDRs/IPs that BYPASS the limit (internal + private ranges by default, so local/cluster traffic is never throttled)
	// Distributed, when true AND a backend is wired via WithDistributedBackend,
	// switches to a CLUSTER-WIDE Redis fixed-window counter so the limit is
	// enforced across all replicas (N pods no longer allow N× the rate). Default
	// false → the per-pod in-process token bucket. Live-tunable like the rest.
	Distributed bool
}

// RateCounter is the minimal shared-counter surface the distributed limiter
// needs: an atomic increment plus a TTL. cache.L2Cache satisfies it structurally,
// so a service passes its Redis L2 without pkg/middleware importing pkg/cache.
type RateCounter interface {
	Incr(ctx context.Context, key string) (int64, error)
	Expire(ctx context.Context, key string, ttl time.Duration) error
}

type RateLimiter struct {
	// cfgFn is read on EVERY request so the config is genuinely live.
	cfgFn func() RateLimitConfig
	log   *slog.Logger

	mu       sync.Mutex
	ips      map[string]*ipEntry
	curRPS   int // the rps/burst the current buckets were built with; a change
	curBurst int // rebuilds them so a live tune takes effect immediately.

	allow clientip.Allowlist // cached CIDR matcher for the bypass allowlist

	// Optional shared backend for distributed (cluster-wide) limiting. scope
	// namespaces the Redis keys per service so tracker + gateway don't collide.
	counter RateCounter
	scope   string
}

// WithDistributedBackend attaches a shared counter so this limiter can enforce a
// cluster-wide limit when RateLimitConfig.Distributed is true. scope namespaces
// the keys per service. Chainable; a nil limiter is a no-op.
func (rl *RateLimiter) WithDistributedBackend(scope string, c RateCounter) *RateLimiter {
	if rl == nil {
		return rl
	}
	rl.scope, rl.counter = scope, c
	return rl
}

// allowDistributed enforces a shared per-IP limit via a Redis fixed-window
// counter keyed to the current second. Coarser than the in-process token bucket
// (a full window's worth is available at the top of each second) but SHARED
// across replicas — the whole point. Fail-OPEN on a backend error, matching the
// platform's Redis posture: a counter outage must not throttle real traffic.
func (rl *RateLimiter) allowDistributed(ctx context.Context, ip string, cfg RateLimitConfig) bool {
	limit := cfg.Burst
	if limit <= 0 {
		limit = cfg.RPS
	}
	key := fmt.Sprintf("ratelimit:%s:%s:%d", rl.scope, ip, time.Now().Unix())
	n, err := rl.counter.Incr(ctx, key)
	if err != nil {
		return true // fail-open
	}
	if n == 1 {
		_ = rl.counter.Expire(ctx, key, 2*time.Second) // outlive the 1s window; self-cleans
	}
	return n <= int64(limit)
}

type ipEntry struct {
	lim  *rate.Limiter
	seen time.Time
}

// NewLiveRateLimiter returns a limiter whose config is resolved from cfgFn on
// every request, so the TierLive <svc>.ratelimit_* keys can be tuned live. RPS
// <= 0 means "disabled" for that window — the limiter passes requests through.
// Never returns nil; the passthrough is decided per request. A background
// janitor evicts idle IPs so the map can't grow unbounded.
func NewLiveRateLimiter(cfgFn func() RateLimitConfig, log *slog.Logger) *RateLimiter {
	rl := &RateLimiter{cfgFn: cfgFn, log: log, ips: make(map[string]*ipEntry)}
	go rl.janitor()
	return rl
}

// NewRateLimiter is the fixed-rate constructor (trustedHops=0 → rightmost XFF,
// no allowlist). Returns nil when rps <= 0 (disabled) — callers treat a nil
// limiter as "no limiting" via the nil-safe Wrap. Retained for callers/tests
// that want a constant rate; services use NewLiveRateLimiter so the TierLive
// keys take effect live.
func NewRateLimiter(rps, burst int, log *slog.Logger) *RateLimiter {
	if rps <= 0 {
		return nil
	}
	return NewLiveRateLimiter(func() RateLimitConfig { return RateLimitConfig{RPS: rps, Burst: burst} }, log)
}

// allowlisted reports whether ip falls in any CIDR/IP on the (comma-separated)
// allowlist — such IPs bypass the limit entirely (cached parse in
// clientip.Allowlist).
func (rl *RateLimiter) allowlisted(ip, allowlist string) bool {
	return rl.allow.Match(ip, allowlist)
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
		cfg := rl.cfgFn()
		if cfg.RPS <= 0 { // disabled live — pass through
			next.ServeHTTP(w, r)
			return
		}
		ip := clientIP(r, cfg.TrustedHops)
		if rl.allowlisted(ip, cfg.Allowlist) { // internal/private/trusted → never throttled
			next.ServeHTTP(w, r)
			return
		}
		// Cluster-wide (Redis) limiting when enabled and a backend is wired; else
		// the per-pod in-process token bucket. A distributed flag with no backend
		// falls through to in-process (safe — never unlimited).
		allowed := true
		if cfg.Distributed && rl.counter != nil {
			allowed = rl.allowDistributed(r.Context(), ip, cfg)
		} else {
			allowed = rl.limiterFor(ip, cfg.RPS, cfg.Burst).Allow()
		}
		if !allowed {
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

// clientIP extracts the caller's IP for rate-limit bucketing — the shared
// right-anchored trusted-proxy parser (see pkg/clientip for the spoof-
// resistance rationale).
func clientIP(r *http.Request, trustedHops int) string {
	return clientip.Resolve(r, trustedHops)
}
