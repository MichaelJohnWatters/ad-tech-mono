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
	if len(p.Segments) > 0 {
		v.Set(ParamSegments, p.SegmentsCSV())
	}

	// Identity signal for the persona's identity type.
	rng := seededRand(p.Name)
	switch p.Identity {
	case IdentityPublisherID:
		v.Set(ParamUserID, "pub-user-"+strconv.FormatUint(uint64(rng.Uint32()), 16))
	case IdentityUID2:
		v.Set(ParamUID2, uid2Token(rng))
	case IdentityHashedEmail:
		v.Set(ParamHashedEmail, hashedEmail(rng))
	}

	// Privacy/regulatory signals for the persona's regime.
	switch p.Regime {
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
	return v
}
