package middleware

import (
	"net/http"
	"net/url"
	"strings"
)

// CSRF blocks cross-site state-changing requests that authenticate via the
// session COOKIE. It's defense-in-depth on top of the cookie's SameSite=Lax:
// Lax already blocks the classic cross-site form POST, and this also catches the
// residual vectors (same-site subdomain, non-form) by requiring the browser's
// Origin (or Referer) to match the request host on unsafe methods.
//
// Deliberately narrow so it can't break legitimate traffic:
//   - Safe methods (GET/HEAD/OPTIONS) are never checked.
//   - A request carrying an Authorization header (Bearer/API-key clients, the
//     e2e harness, server-to-server) is exempt — a browser never attaches those
//     automatically, so they aren't CSRF-able.
//   - A request WITHOUT the session cookie is exempt (nothing to ride).
//   - Only when Origin/Referer is PRESENT and cross-host do we reject; an absent
//     header is allowed (non-browser cookie clients don't send one, and Lax
//     still covers the browser case).
func CSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		if r.Header.Get("Authorization") != "" {
			next.ServeHTTP(w, r) // token auth — not browser-CSRF-able
			return
		}
		if _, err := r.Cookie(SessionCookieName); err != nil {
			next.ServeHTTP(w, r) // no session cookie — nothing to forge
			return
		}
		origin := r.Header.Get("Origin")
		if origin == "" {
			origin = r.Header.Get("Referer")
		}
		if origin != "" {
			if u, err := url.Parse(origin); err == nil && u.Host != "" && !sameHost(u.Host, r.Host) {
				http.Error(w, `{"error":"cross-site request blocked"}`, http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// sameHost compares two Host values ignoring the port (behind an ingress the
// request Host and the browser Origin may differ only by an implied :443).
func sameHost(a, b string) bool {
	return strings.EqualFold(hostOnly(a), hostOnly(b))
}

func hostOnly(h string) string {
	if i := strings.IndexByte(h, ':'); i >= 0 {
		return h[:i]
	}
	return h
}
