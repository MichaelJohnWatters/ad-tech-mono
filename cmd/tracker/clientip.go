package main

import (
	"net/http"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clientip"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
)

// newClientIPFn returns the fraud-check client-IP resolver — the shared
// right-anchored trusted-proxy parser (pkg/clientip), so a bot can't dodge the
// IP blocklist by prepending forged X-Forwarded-For entries, and the tracker's
// fraud IP agrees with the SSP's household IP for the same viewer. The tracker
// sits behind Traefik (and, in prod, a CDN/LB), so r.RemoteAddr is the proxy,
// not the client. The hop count is read live per call (tracker.trusted_proxy_hops)
// so a topology change (adding a CDN) takes effect without a restart.
func newClientIPFn(cfg *config.Config) func(*http.Request) string {
	return func(r *http.Request) string {
		return clientip.Resolve(r, keys.Tracker.TrustedProxyHops.Get(cfg))
	}
}

// newEndUserIPFn returns the END-USER IP resolver for the retargeting pixel —
// the input to household-id derivation (guest-cart household enrollment).
// Mirrors the SSP's resolver exactly: base = the trusted-proxy parse above;
// an explicit ?ip= override is honoured ONLY when the caller sits inside
// tracker.ip_override_allowlist (private ranges by default). Without the
// gate a public browser could rotate ?ip= and enroll arbitrary households
// into an advertiser's retargeting audience.
func newEndUserIPFn(cfg *config.Config) func(*http.Request) string {
	var allow clientip.Allowlist
	return func(r *http.Request) string {
		caller := clientip.Resolve(r, keys.Tracker.TrustedProxyHops.Get(cfg))
		if override := r.URL.Query().Get("ip"); override != "" &&
			allow.Match(caller, keys.Tracker.IPOverrideAllowlist.Get(cfg)) {
			return override
		}
		return caller
	}
}
