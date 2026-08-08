package main

import (
	"net/http"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clientip"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
)

// newEndUserIPFn returns the END-USER IP resolver — the single source for
// household-id derivation and the identity fingerprint, so the two can never
// disagree for one viewer. Two tiers:
//
//   - Base: the shared right-anchored trusted-proxy parse (pkg/clientip,
//     hops from ssp.trusted_proxy_hops, read live per call). A browser
//     prepending forged X-Forwarded-For entries cannot move its household.
//
//   - ?ip= override: server-side callers that legitimately know the device IP
//     (SSAI, server-side publisher tags, the simulator/e2e harness) forward it
//     as ?ip= because the SSP otherwise sees THEIR address, not the viewer's.
//     Honoured ONLY when the caller — the connection-level, trusted-proxy-
//     resolved address — is inside ssp.ip_override_allowlist (private ranges
//     by default). Without the gate any public browser could rotate ?ip= to
//     mint a fresh household per request and walk straight through household
//     frequency caps.
func newEndUserIPFn(cfg *config.Config) func(*http.Request) string {
	var allow clientip.Allowlist
	return func(r *http.Request) string {
		caller := clientip.Resolve(r, keys.SSP.TrustedProxyHops.Get(cfg))
		if override := r.URL.Query().Get("ip"); override != "" &&
			allow.Match(caller, keys.SSP.IPOverrideAllowlist.Get(cfg)) {
			return override
		}
		return caller
	}
}
