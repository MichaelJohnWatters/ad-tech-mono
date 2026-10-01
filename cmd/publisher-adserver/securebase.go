package main

import (
	"net/http"
	"net/url"
	"strings"
)

// secureBase decides, per inbound serve request, which browser-reachable public
// base (scheme+host) the served ad's MEDIA and tracking BEACONS should use.
//
// The demo publisher sites are served over HTTPS behind the Traefik ingress
// (https://viewtube.adtech.local etc.). A browser on an HTTPS page blocks
// http:// sub-resources (mixed content), so an ad whose MediaFile / impression
// pixel / click URL is http://localhost:8080/... never loads and no impression
// records. Traefik stamps X-Forwarded-Proto: https on requests it forwards from
// the ingress, so that header is our signal that the ad will render on an HTTPS
// page and must therefore use an HTTPS, browser-reachable base.
//
// The http://localhost path — the pub-simulator via the bridge, the e2e
// harness, CI — has no X-Forwarded-Proto: https (no /etc/hosts, no :443
// tunnel), so it falls through to the current base unchanged, byte-for-byte.
// This is the hard constraint: only the ingress (https) path is rewritten.
func secureRequest(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// secureTrackerBase returns the tracker/beacon base to bake into this request's
// signed beacons: the configured secure base on an HTTPS ingress request, else
// the current (http) tracker base unchanged. Because beacons are HMAC-signed
// over the PATH + sorted PARAMS only (pkg/adserving/signing.go), swapping the
// scheme+host base before signing leaves the signed material identical — the
// signature stays valid.
func secureTrackerBase(r *http.Request, trackerURL, secureBase string) string {
	if secureRequest(r) {
		return secureBase
	}
	return trackerURL
}

// rewriteHostIfSecure rewrites an already-built absolute URL's scheme+host to
// the secure base when the request arrived over the HTTPS ingress, preserving
// the PATH + QUERY (and fragment) exactly. Used for URLs we did NOT build
// in-process and so cannot re-base before signing: the creative media URL
// (winner.MediaURL from the DB) and the display passthrough's already-signed
// beacon URLs. Keeping path+query byte-identical means any signature over them
// still validates — only the un-signed scheme+host changes.
//
// Non-https requests, empty inputs, relative URLs, or an unparseable secure
// base all pass the raw URL through unchanged.
func rewriteHostIfSecure(r *http.Request, raw, secureBase string) string {
	if !secureRequest(r) {
		return raw
	}
	return rewriteHost(raw, secureBase)
}

// rewriteHost swaps raw's scheme+host for secureBase's, keeping path/query/
// fragment. Pass-through on any parse failure or a relative raw URL.
func rewriteHost(raw, secureBase string) string {
	if raw == "" {
		return raw
	}
	sb, err := url.Parse(secureBase)
	if err != nil || sb.Host == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw // relative / unparseable — nothing to re-host
	}
	u.Scheme = sb.Scheme
	u.Host = sb.Host
	return u.String()
}

// rewriteBaseIfSecure replaces every occurrence of the current public base
// (oldBase, e.g. http://localhost:8080) with the secure base inside an
// already-rendered HTML/JSON body, but only on an HTTPS ingress request. This
// covers the display serve path, whose HTML (creative <img>/asset src +
// baked-in impression/click beacons) and sibling beacon URLs are built
// downstream (ad server / SSP) and returned to us as opaque strings. The
// replacement is scheme+host only (oldBase has no path), so signed beacon
// path+params are untouched and the HMAC stays valid. URL-ENCODED occurrences
// (e.g. a signed redir=http%3A%2F%2Flocalhost%3A8080... param) do not match the
// literal oldBase and are correctly left alone.
//
// No-op when the request isn't https, or when oldBase/secureBase is empty or
// they're already equal.
func rewriteBaseIfSecure(r *http.Request, body, oldBase, secureBase string) string {
	if !secureRequest(r) || oldBase == "" || secureBase == "" || oldBase == secureBase {
		return body
	}
	return strings.ReplaceAll(body, oldBase, secureBase)
}
