package keys

import (
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

var ssaiSet = config.NewKeySet(constants.ServiceSSAI)

// SSAISchema is the SSAI schema — passed to config.Setup at boot.
func SSAISchema() []config.SchemaEntry {
	return append(ssaiSet.Entries(), transcodeSharedEntries()...)
}

// SSAI holds the SSAI config keys.
var SSAI = struct {
	Port                config.StringKey
	SSPURL              config.StringKey
	TrackerURL          config.StringKey
	PublicURL           config.StringKey
	AdServerURL         config.StringKey
	AdPlacementID       config.StringKey
	OriginURL           config.StringKey
	TranscoderURL       config.StringKey
	MaxPodAds           config.IntKey
	SlateCreativeID     config.StringKey
	SlateMediaURL       config.StringKey
	CreativesBucket     config.StringKey
	TimedMetadata       config.BoolKey
	OmidVerificationURL config.StringKey
	OmidVendor          config.StringKey
	DASHMultiRung       config.BoolKey

	// URL/Port are env/manifest territory by design — Raw, not in the schema.
	URL config.StringKey
}{
	Port:                ssaiSet.String("ssai.port", routes.PortSSAI, config.TierStatic, "Port the SSAI stitcher listens on.", config.Since("v1.5")),
	SSPURL:              ssaiSet.String("ssai.ssp_url", routes.DefaultSSPURL, config.TierStatic, "SSP the stitcher calls to run a per-ad-break auction (channel=video). Same endpoint the publisher ad server uses.", config.Since("v1.5")),
	TrackerURL:          ssaiSet.String("ssai.tracker_url", routes.DefaultTrackerURL, config.TierStatic, "Base URL the stitcher fires impression + quartile beacons at, server-side, on behalf of the player. MUST be reachable from the stitcher.", config.Since("v1.5")),
	PublicURL:           ssaiSet.String("ssai.public_url", routes.DefaultGatewayURL, config.TierStatic, "Browser-reachable origin the stitched manifest tells the player to fetch ad segments from (points back at the SSAI segment beacon endpoint via the gateway).", config.Since("v1.5")),
	AdServerURL:         ssaiSet.String("ssai.adserver_url", routes.DefaultAdServerURL, config.TierStatic, "Ad server the stitcher calls to RECORD the frequency cap when it stitches an ad (the SSP only PEEKed at the auction, so video/audio impressions count at stitch time, not at the serve decision). Empty disables cap recording.", config.Since("v1.20")),
	AdPlacementID:       ssaiSet.String("ssai.ad_placement_id", "pl-sim-video", config.TierLive, "Placement id used for the per-break auction when the manifest request doesn't specify one.", config.Since("v1.5")),
	OriginURL:           ssaiSet.String("ssai.origin_url", "", config.TierLive, "Origin HLS manifest the stitcher fetches and rewrites (the publisher's content playlist, with #EXT-X-CUE-OUT/CUE-IN ad-break markers). Empty (default) uses the built-in sample manifest; set it (or pass ?origin=<url>) to stitch a real origin. The request's ?origin= param overrides this.", config.Since("v1.6")),
	TranscoderURL:       ssaiSet.String("ssai.transcoder_url", routes.DefaultTranscoderURL, config.TierStatic, "Runtime ad-conditioning service. On a break the stitcher asks it (cache-only) for the winning ad's segments, byte-compatible with the content; on a miss it warms the conditioner async and keeps content while warming. Empty disables conditioning.", config.Since("v1.6")),
	MaxPodAds:           ssaiSet.Int("ssai.max_pod_ads", "4", config.TierLive, "Maximum ads stitched into a single avail (ad pod). The stitcher runs back-to-back auctions until the break duration is filled or this many ads are placed. 1 = single-ad breaks (no pods).", config.Since("v1.6")),
	SlateCreativeID:     ssaiSet.String("ssai.slate_creative_id", "", config.TierLive, "Creative id of a house/slate clip conditioned across the ladder. When an avail can't be filled (no bid, or the winner isn't conditioned yet), the stitcher splices this slate (cache-only) instead of dropping to content. Empty disables slate (keeps content).", config.Since("v1.6")),
	SlateMediaURL:       ssaiSet.String("ssai.slate_media_url", "", config.TierLive, "Media URL of the slate creative, used to warm-condition the slate on a cache miss so it's ready next time.", config.Since("v1.6")),
	CreativesBucket:     ssaiSet.String("ssai.creatives_bucket", "adtech-creatives", config.TierStatic, "Object-store bucket the stitcher reads /v1/creatives origin manifests from (the in-cluster stitcher can't reach the browser-facing gateway host).", config.Since("v1.6")),
	TimedMetadata:       ssaiSet.Bool("ssai.timed_metadata", "false", config.TierLive, "Emit DASH EventStream quartile timed-metadata on ad periods so the player (dash.js) can attribute quartiles at playback time (client-side), complementing the server-side segment beacons. Off by default to avoid double-counting.", config.Since("v1.6")),
	OmidVerificationURL: ssaiSet.String("ssai.omid_verification_url", "", config.TierLive, "Open Measurement (OMID) verification-resource JS URL delivered to the SSAI player via the manifest (DASH EventStream urn:adtech:ssai:omid + HLS DATERANGE X-OMID-RESOURCE), so an OM-SDK player can measure viewability of stitched ads. Empty = no OMID. Self-host the resource (no third-party).", config.Since("v1.6")),
	OmidVendor:          ssaiSet.String("ssai.omid_vendor", "adtech-om", config.TierLive, "OMID verification vendor key advertised alongside ssai.omid_verification_url.", config.Since("v1.6")),
	DASHMultiRung:       ssaiSet.Bool("ssai.dash_multi_rung", "false", config.TierLive, "DASH: emit a multi-Representation ABR MPD (one rung per Representation) instead of single-rung. Ads are decided once per break and kept only when conditioned on every rung (else warm + drop for the request), so periods stay aligned. Off by default; single-rung is the proven path.", config.Since("v1.6")),
	URL:                 config.RawString("ssai.url", routes.DefaultSSAIURL),
}
