package middleware

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tracing"
)

// actAsCookieName is the portal switcher's selected act-as account. The proxy
// resolves it (validated via auth.CanAccessAccount) the same as the
// X-Act-As-Account header.
const actAsCookieName = "act_as_account"

// ActAsTarget returns the caller's requested act-as target — the
// X-Act-As-Account header (API clients) or the act_as_account cookie (the
// portal switcher, which can't set headers on navigations). Header wins.
// Empty when the caller isn't acting-as/impersonating.
func ActAsTarget(r *http.Request) string {
	if h := strings.TrimSpace(r.Header.Get(constants.HeaderActAs)); h != "" {
		return h
	}
	if ck, err := r.Cookie(actAsCookieName); err == nil {
		return strings.TrimSpace(ck.Value)
	}
	return ""
}

// ParseActAsTarget splits an act-as value into (accountType, accountID). The
// staff impersonation switcher sets "advertiser:<id>" / "publisher:<id>" (it
// knows the target's type); the agency switcher sets a bare "<id>" which
// defaults to advertiser (agencies only manage advertisers). An unrecognised
// type prefix also defaults to advertiser.
func ParseActAsTarget(s string) (auth.AccountType, string) {
	if i := strings.IndexByte(s, ':'); i > 0 {
		if t := auth.AccountType(s[:i]); t == auth.AccountAdvertiser || t == auth.AccountPublisher {
			return t, s[i+1:]
		}
		return auth.AccountAdvertiser, s[i+1:]
	}
	return auth.AccountAdvertiser, s
}

// trustedIdentityHeaders are the request headers the gateway injects AFTER it
// has authenticated a caller — from JWT claims (ReverseProxy's ClaimsFromContext
// block) or a validated publisher lookup (injectTraceScope). Internal services
// trust them implicitly for tenant scoping and redaction, so an inbound client
// must never be able to set them: a request arriving with `X-Account-Type: staff`
// would otherwise be forwarded verbatim by any UNAUTHENTICATED pass-through proxy
// (Swagger try-it-out, /v1/reporting/, /v1/billing/…) and read as a platform
// operator downstream — an un-scoped, un-redacted cross-tenant read with no auth.
var trustedIdentityHeaders = []string{
	constants.HeaderAccountID,
	constants.HeaderAccountType,
	constants.HeaderUserID,
	constants.HeaderPublisherID,
	constants.HeaderActAs,
}

// StripClientIdentityHeaders deletes the gateway-injected identity headers from
// an inbound request so a client can't spoof them. Mount it OUTERMOST on the
// gateway — before auth and any header-injecting middleware — so the strip runs
// first and the legitimate setters (ReverseProxy's claims block, injectTraceScope)
// re-add validated values further in. Requests that reach a pass-through proxy
// with no injector in front then carry no identity headers at all, so the
// downstream service denies rather than trusting a forged value.
func StripClientIdentityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, h := range trustedIdentityHeaders {
			r.Header.Del(h)
		}
		next.ServeHTTP(w, r)
	})
}

// ReverseProxy creates a simple reverse proxy handler that forwards requests
// to a backend service. Used by the gateway to proxy API calls to internal services.
func ReverseProxy(target string, log *slog.Logger) http.Handler {
	// CheckRedirect returns ErrUseLastResponse so the gateway forwards
	// upstream 3xx responses verbatim instead of following them. Critical
	// for the tracker click flow where the 302 location points at an
	// external advertiser domain the gateway can't (and shouldn't) reach.
	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Build upstream URL
		upstreamURL := target + r.URL.Path
		if r.URL.RawQuery != "" {
			upstreamURL += "?" + r.URL.RawQuery
		}

		// Create upstream request
		upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, r.Body)
		if err != nil {
			log.Error("proxy: failed to create request", "error", err)
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}

		// Copy headers
		for k, vv := range r.Header {
			for _, v := range vv {
				upstreamReq.Header.Add(k, v)
			}
		}

		// Inject the caller's identity from auth claims for multi-tenancy.
		// Account type lets the internal service distinguish an end-customer
		// session (scope reads/writes to their account) from a platform
		// operator (unscoped) — see middleware.CallerScope.
		claims := ClaimsFromContext(r.Context())
		if claims != nil {
			acctID := claims.AccountID
			acctType := string(claims.AccountType)
			// Act-as / impersonation: an agency may target one of its managed
			// advertiser accounts; staff/admin may impersonate ANY account
			// (advertiser or publisher). auth.CanAccessAccount is the authority —
			// it already encodes "agency → managed set" and "staff → any". When a
			// target is set and permitted, forward it (with the target's real
			// type) as the effective tenant so downstream scoping applies to it.
			// A target the caller can't access is rejected, never silently ignored.
			if target := ActAsTarget(r); target != "" {
				tType, tID := ParseActAsTarget(target)
				if !auth.CanAccessAccount(claims, tID) {
					http.Error(w, "forbidden: cannot act as that account", http.StatusForbidden)
					return
				}
				acctID = tID
				acctType = string(tType)
				log.Info("act-as", "caller", claims.AccountID, "caller_type", claims.AccountType, "as_account", tID, "as_type", tType)
			}
			upstreamReq.Header.Set(constants.HeaderAccountID, acctID)
			upstreamReq.Header.Set(constants.HeaderAccountType, acctType)
			upstreamReq.Header.Set(constants.HeaderUserID, claims.UserID)
		}
		// Never let an inbound act-as header leak past the gateway — the
		// gateway is the only place authorised to resolve it.
		upstreamReq.Header.Del(constants.HeaderActAs)

		// Propagate the W3C trace context (traceparent) to the upstream so the
		// downstream service continues THIS trace instead of forking a new one.
		// The gateway's tracing.HTTPMiddleware put the authoritative span
		// context in r.Context(); a request that arrived without a traceparent
		// (a browser fetch, a raw curl) would otherwise start a fresh trace at
		// each proxied hop — so the exchange's auction + AuctionWinEvent would
		// land under a different trace_id than the served impression, breaking
		// end-to-end continuity. Injecting from context makes the whole request
		// (auction → win → serve → impression) share one trace_id.
		tracing.InjectHTTP(r.Context(), upstreamReq)

		// Forward
		start := time.Now()
		resp, err := client.Do(upstreamReq)
		if err != nil {
			log.Error("proxy: upstream failed", "target", target, "error", err)
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		// Copy response headers, but strip any Access-Control-* headers
		// the upstream set. The gateway's own CORS middleware is the
		// authoritative source for these — copying upstream's values
		// would emit duplicates (Access-Control-Allow-Origin: *, *) and
		// strict CORS clients (notably the IMA SDK iframe) treat
		// concatenated wildcards as a mismatch with the request origin,
		// failing the resource-sharing check post-fetch.
		for k, vv := range resp.Header {
			if strings.HasPrefix(strings.ToLower(k), "access-control-") {
				continue
			}
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)

		log.Debug("proxy",
			"method", r.Method,
			"path", r.URL.Path,
			"target", target,
			"status", resp.StatusCode,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// StripPrefix removes a path prefix before proxying.
// E.g. StripPrefix("/v1/api/campaigns", proxy) strips "/v1/api/campaigns"
// so "/v1/api/campaigns/li-001" becomes "/li-001" at the upstream.
func StripPrefix(prefix string, handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A request for exactly the prefix strips to "" — leave it empty.
		// ReverseProxy targets already carry the full upstream base path
		// (e.g. dspURL + /v1/dsp/campaigns), so forcing "/" here produced
		// ".../v1/dsp/campaigns/" upstream, which ServeMux routes to the
		// by-ID subtree handler (empty id → 404) instead of the collection.
		r.URL.Path = strings.TrimPrefix(r.URL.Path, prefix)
		handler.ServeHTTP(w, r)
	})
}
