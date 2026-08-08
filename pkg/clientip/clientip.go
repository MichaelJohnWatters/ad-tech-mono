// Package clientip resolves the originating client IP for requests arriving
// through the platform's reverse-proxy chain. It is the ONE parser for every
// consumer of "who is this viewer" — rate limiting, fraud checks, household-id
// derivation, identity fingerprinting — so the tiers (X-Forwarded-For →
// X-Real-IP → RemoteAddr) and the spoof-resistance rules can't drift between
// services: the tracker's fraud IP and the SSP's household IP must be the SAME
// address for the same viewer, or a capped household still gets fraud-scored
// under a different identity (and vice versa).
package clientip

import (
	"net"
	"net/http"
	"strings"
	"sync"
)

// Resolve extracts the client IP. trustedHops is the number of trusted reverse
// proxies in front of the app; the real client is that many entries from the
// RIGHT of X-Forwarded-For, because each proxy APPENDS the address it received
// the connection from. Taking a right-anchored entry is what makes the result
// spoof-resistant: a client can PREPEND fake X-Forwarded-For values, but it
// can't forge the entries a trusted proxy appended after them.
//
//   - trustedHops=0 (default): the app's direct upstream (our ingress) is the
//     only trusted hop → use the rightmost XFF entry (the client the ingress saw).
//   - trustedHops=1: a CDN sits in front of the ingress → skip the ingress
//     entry, use the next one (the client the CDN saw). And so on.
//
// Falls back to X-Real-IP, then RemoteAddr (port stripped, so blocklist exact
// matches and CIDR checks see a bare IP), when XFF is absent (direct/local).
func Resolve(r *http.Request, trustedHops int) string {
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

// Allowlist is a cached CIDR/IP matcher for comma-separated allowlist config
// strings. The parse is cached and only redone when the string changes, so
// Match is cheap enough for hot paths reading a live config key per request.
// The zero value is ready to use; safe for concurrent use.
type Allowlist struct {
	mu   sync.Mutex
	raw  string
	nets []*net.IPNet
}

// Match reports whether ip falls in any CIDR/IP of the (comma-separated)
// cidrs list. A bare IP entry is treated as a /32 (or /128). Malformed
// entries are skipped. An empty list or unparseable ip never matches.
func (a *Allowlist) Match(ip, cidrs string) bool {
	if cidrs == "" {
		return false
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	a.mu.Lock()
	if cidrs != a.raw {
		a.raw = cidrs
		a.nets = a.nets[:0]
		for _, tok := range strings.Split(cidrs, ",") {
			tok = strings.TrimSpace(tok)
			if tok == "" {
				continue
			}
			if !strings.Contains(tok, "/") {
				if strings.Contains(tok, ":") {
					tok += "/128"
				} else {
					tok += "/32"
				}
			}
			if _, n, err := net.ParseCIDR(tok); err == nil {
				a.nets = append(a.nets, n)
			}
		}
	}
	nets := a.nets
	a.mu.Unlock()
	for _, n := range nets {
		if n.Contains(parsed) {
			return true
		}
	}
	return false
}
