package middleware

import "net/http"

// SecurityHeaders wraps a handler with a baseline set of response security
// headers. Applied to the browser-facing gateway.
//
//   - Strict-Transport-Security: only when the request arrived over HTTPS (direct
//     TLS or X-Forwarded-Proto=https behind the ingress) — so local plain-HTTP dev
//     doesn't get pinned to HTTPS. Tells browsers to use HTTPS for a year.
//   - X-Content-Type-Options: nosniff — stop MIME-sniffing (defense vs. content
//     injection served with the wrong type).
//   - X-Frame-Options: SAMEORIGIN — the dashboard can't be framed cross-origin
//     (clickjacking); it can still embed same-origin/allowlisted iframes itself.
//   - Referrer-Policy: strict-origin-when-cross-origin — don't leak full URLs
//     (which can carry ids/tokens) to third parties.
//
// NOTE: a Content-Security-Policy is intentionally NOT set here yet — the portal
// templates use inline <script>/<style>, which a strict CSP would break; adding
// one needs a nonce/hash pass over the templates (tracked separately). The above
// headers are safe to ship as-is.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		if RequestIsSecure(r) {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		next.ServeHTTP(w, r)
	})
}
