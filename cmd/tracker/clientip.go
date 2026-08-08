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
