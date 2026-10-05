// IAB bracket-macro support (VAST 4.2 §6 "Macros").
//
// VAST macros are square-bracket tokens ([ERRORCODE], [CACHEBUSTING], ...)
// embedded verbatim in tracking/error URIs. Unlike the platform's ${...}
// macros (pkg/adserving, expanded SERVER-side before signing), bracket
// macros are substituted by the PLAYER at fire time via literal text
// replacement — the server emits the token and never sees the value until
// the beacon arrives. ExpandURIMacros below is the player-side half, used
// by our simulated players; the server-side half is just string-emitting
// the tokens (see adserving.AppendClientMacroParams).
package vast

import (
	"fmt"
	"math/rand"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrorUndefined is the VAST error code a server records when a player
// could not substitute [ERRORCODE] — the spec says a macro-incapable
// player leaves the token in place and the server treats it as 900.
const ErrorUndefined = 900

// errorCodeNames enumerates the VAST 4.2 §2.3.6.3 error-code table. The
// name strings double as debug labels in logs; membership is the
// validity check (IsValidErrorCode).
var errorCodeNames = map[int]string{
	100: "XML parsing error",
	101: "VAST schema validation error",
	102: "VAST version of response not supported",
	200: "trafficking error (wrong ad type)",
	201: "different linearity expected",
	202: "different duration expected",
	203: "different size expected",
	204: "ad category required but not provided",
	205: "inline category violates wrapper BlockedAdCategories",
	206: "ad break shortened",
	300: "general wrapper error",
	301: "timeout of VAST URI",
	302: "wrapper limit reached",
	303: "no VAST response after wrapper(s)",
	304: "inline ad failed to display within time limit",
	400: "general linear error",
	401: "media file not found",
	402: "timeout of MediaFile URI",
	403: "no supported MediaFile found",
	405: "problem displaying MediaFile",
	406: "mezzanine required but not provided",
	407: "mezzanine still downloading",
	408: "conditional ad rejected",
	409: "interactive unit not executed",
	410: "verification unit not executed",
	500: "general NonLinearAds error",
	501: "nonlinear dimensions do not align",
	502: "unable to fetch NonLinear resource",
	503: "no supported NonLinear resource",
	600: "general CompanionAds error",
	601: "companion dimensions do not fit",
	602: "unable to display required companion",
	603: "unable to fetch companion resource",
	604: "no supported companion resource",
	900: "undefined error",
	901: "general VPAID error",
	902: "general InteractiveCreativeFile error",
}

// IsValidErrorCode reports whether c is an enumerated VAST 4.2 error code.
func IsValidErrorCode(c int) bool {
	_, ok := errorCodeNames[c]
	return ok
}

// ErrorCodeName returns the spec label for an error code ("" if unknown).
func ErrorCodeName(c int) string { return errorCodeNames[c] }

// URIMacroValues carries the player-side values substituted into bracket
// macros. Zero values substitute sensibly (ErrorCode 0 → "0", zero
// playhead → "00:00:00.000"); Now defaults to time.Now() and Rand to
// math/rand so callers only set what they have.
type URIMacroValues struct {
	ErrorCode       int
	AdPlayhead      time.Duration
	ContentPlayhead time.Duration
	AssetURI        string
	Now             time.Time
	Rand            func() int64
}

// ExpandURIMacros performs the player-side literal replacement of the
// bracket macros this platform emits. Unknown bracket tokens are left
// intact (per spec a player only substitutes macros it supports; servers
// must tolerate literals — see the tracker's [ERRORCODE]→900 rule).
func ExpandURIMacros(uri string, v URIMacroValues) string {
	if !strings.Contains(uri, "[") {
		return uri
	}
	now := v.Now
	if now.IsZero() {
		now = time.Now()
	}
	randFn := v.Rand
	if randFn == nil {
		randFn = rand.Int63
	}
	pairs := [...]string{
		"[ERRORCODE]", strconv.Itoa(v.ErrorCode),
		// 8-digit random number, per the spec's own examples.
		"[CACHEBUSTING]", fmt.Sprintf("%08d", randFn()%100_000_000),
		// VAST 4.x [TIMESTAMP] is ISO 8601 with milliseconds — NOT unix
		// seconds (that's the platform's ${TIMESTAMP}). URL-encoded because
		// it lands in a query param and carries ':' and '+'.
		"[TIMESTAMP]", url.QueryEscape(now.Format("2006-01-02T15:04:05.000-07:00")),
		"[ADPLAYHEAD]", playheadString(v.AdPlayhead),
		"[CONTENTPLAYHEAD]", playheadString(v.ContentPlayhead),
		"[ASSETURI]", url.QueryEscape(v.AssetURI),
	}
	return strings.NewReplacer(pairs[:]...).Replace(uri)
}

// playheadString formats a playhead position as HH:MM:SS.mmm (the VAST
// macro value format — always with millis, unlike Duration.MarshalText
// which drops them when zero). URL-safe as-is apart from ':', which we
// escape for the same query-param reason as [TIMESTAMP].
func playheadString(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	h := int(d / time.Hour)
	d -= time.Duration(h) * time.Hour
	m := int(d / time.Minute)
	d -= time.Duration(m) * time.Minute
	s := int(d / time.Second)
	d -= time.Duration(s) * time.Second
	ms := int(d / time.Millisecond)
	return url.QueryEscape(fmt.Sprintf("%02d:%02d:%02d.%03d", h, m, s, ms))
}
