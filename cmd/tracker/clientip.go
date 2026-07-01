package main

import (
	"net"
	"net/http"
	"strings"
)

// clientIP returns the originating client IP for fraud checks. The tracker
// sits behind Traefik (and, in prod, a CDN/LB), so r.RemoteAddr is the proxy,
// not the client — the real client is the first hop in X-Forwarded-For.
//
// Precedence: X-Forwarded-For (first entry) → X-Real-IP → RemoteAddr. The
// RemoteAddr fallback is "host:port"; we strip the port so blocklist exact
// matches and datacenter-CIDR checks (net.ParseIP) see a bare IP.
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
