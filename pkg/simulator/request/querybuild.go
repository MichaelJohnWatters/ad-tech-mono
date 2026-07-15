package request

import (
	"hash/fnv"
	"math/rand"
	"net/url"
	"strconv"
)

// seededRand returns a rand.Rand seeded deterministically from key, so a given
// persona renders stable identity/consent values across calls (handy for the
// browser path where the same persona should map to the same synthetic user).
func seededRand(key string) *rand.Rand {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	return rand.New(rand.NewSource(int64(h.Sum64())))
}

// Canonical query-parameter names the SSP (/v1/ssp/serve) and publisher
// ad-server (/v1/pubad/*) read to build a production-like OpenRTB request on
// the browser-driven paths. The browser (or any publisher client) can't build
// OpenRTB itself, so it passes these signals through and the server assembles
// the request. Kept here so client and server share one spelling.
const (
	ParamPlacementID = "placement_id"
	ParamChannel     = "channel"
	ParamGeo         = "geo"
	ParamRegion      = "region"
	ParamDevice      = "device"
	ParamOS          = "os"
	ParamIP          = "ip" // stable persona client IP → SSP household derivation
	ParamUserID      = "user_id"
	ParamUID2        = "uid2"
	ParamHashedEmail = "hashed_email"
	ParamSegments    = "segments" // comma-separated
	ParamConsent     = "consent"  // TCF string
	ParamGDPR        = "gdpr"     // "1"/"0"
	ParamUSPrivacy   = "us_privacy"
	ParamGPP         = "gpp"
	ParamGPPSID      = "gpp_sid"
	ParamGPC         = "gpc"   // "1"/"0"
	ParamCOPPA       = "coppa" // "1"/"0"
)

// QueryParams renders a persona + placement + channel as the query parameters
// the SSP/pubad endpoints understand. It mirrors what Build puts into the
// OpenRTB request, so the browser path and the direct-POST path exercise the
// same downstream decisions. placementID is passed explicitly because the
// external placement key (not the UUID) is what those endpoints expect.
func (p Persona) QueryParams(placementID string, ch Channel) url.Values {
	v := url.Values{}
	if placementID != "" {
		v.Set(ParamPlacementID, placementID)
	}
	if ch != "" {
		v.Set(ParamChannel, string(ch))
	}
	if p.Geo != "" {
		v.Set(ParamGeo, p.Geo)
	}
	if p.Region != "" {
		v.Set(ParamRegion, p.Region)
	}
	if p.Device != "" {
		v.Set(ParamDevice, p.Device)
	}
	if p.OS != "" {
		v.Set(ParamOS, p.OS)
	}
	if p.IP != "" {
		v.Set(ParamIP, p.IP)
	}
	if len(p.Segments) > 0 {
		v.Set(ParamSegments, p.SegmentsCSV())
	}
	// Consent + identity encoding is the single-source-of-truth part, shared
	// with the web UI via the /v1/sim/realism endpoint (see RealismParams).
	mergeValues(v, RealismParams(p.Regime, p.Identity, p.Name))
	return v
}

// RealismParams encodes the privacy + identity signals for a consent regime and
// identity type into query params. This is THE single source of truth for the
// fiddly TCF / GPP / US-privacy / UID2 encoding — used by the CLI (via
// Persona.QueryParams) and by the web UI (which fetches it from the
// /v1/sim/realism endpoint instead of re-implementing it in JS). seed makes the
// synthetic identity values deterministic per caller so the same selection maps
// to the same synthetic user, matching the CLI's per-persona stability.
func RealismParams(regime Regime, identity Identity, seed string) url.Values {
	v := url.Values{}
	rng := seededRand(seed)
	applyIdentityParams(v, identity, rng)
	applyRegimeParams(v, regime)
	return v
}

func applyIdentityParams(v url.Values, identity Identity, rng *rand.Rand) {
	switch identity {
	case IdentityPublisherID:
		v.Set(ParamUserID, "pub-user-"+strconv.FormatUint(uint64(rng.Uint32()), 16))
	case IdentityUID2:
		v.Set(ParamUID2, uid2Token(rng))
	case IdentityHashedEmail:
		v.Set(ParamHashedEmail, hashedEmail(rng))
	case IdentityAnonymous:
		// no identifier
	}
}

func applyRegimeParams(v url.Values, regime Regime) {
	switch regime {
	case RegimeGDPRConsented:
		v.Set(ParamGDPR, "1")
		v.Set(ParamConsent, tcfConsentString)
	case RegimeGDPRNoConsent:
		v.Set(ParamGDPR, "1")
	case RegimeCCPAOptOut:
		v.Set(ParamUSPrivacy, "1YYN")
	case RegimeGPC:
		v.Set(ParamGPC, "1")
	case RegimeCOPPA:
		v.Set(ParamCOPPA, "1")
	case RegimeGPPOptOut:
		v.Set(ParamGPP, gppUSNationalOptOut())
		v.Set(ParamGPPSID, "7")
	case RegimeUSClear:
		v.Set(ParamUSPrivacy, "1YNN")
	}
}

func mergeValues(dst, src url.Values) {
	for k, vs := range src {
		for _, val := range vs {
			dst.Set(k, val)
		}
	}
}
