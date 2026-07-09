package main

import (
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// ssaiSchema is the per-service config for the SSAI stitcher.
var ssaiSchema = []config.SchemaEntry{
	{Key: "ssai.port", Type: "string", Tier: config.TierStatic, Default: routes.PortSSAI, Description: "Port the SSAI stitcher listens on.", Service: constants.ServiceSSAI, Since: "v1.5"},
	{Key: "ssai.ssp_url", Type: "string", Tier: config.TierStatic, Default: routes.DefaultSSPURL, Description: "SSP the stitcher calls to run a per-ad-break auction (channel=video). Same endpoint the publisher ad server uses.", Service: constants.ServiceSSAI, Since: "v1.5"},
	{Key: "ssai.tracker_url", Type: "string", Tier: config.TierStatic, Default: routes.DefaultTrackerURL, Description: "Base URL the stitcher fires impression + quartile beacons at, server-side, on behalf of the player. MUST be reachable from the stitcher.", Service: constants.ServiceSSAI, Since: "v1.5"},
	{Key: "ssai.public_url", Type: "string", Tier: config.TierStatic, Default: routes.DefaultGatewayURL, Description: "Browser-reachable origin the stitched manifest tells the player to fetch ad segments from (points back at the SSAI segment beacon endpoint via the gateway).", Service: constants.ServiceSSAI, Since: "v1.5"},
	{Key: "ssai.ad_placement_id", Type: "string", Tier: config.TierLive, Default: "pl-sim-video", Description: "Placement id used for the per-break auction when the manifest request doesn't specify one.", Service: constants.ServiceSSAI, Since: "v1.5"},
	{Key: "ssai.origin_url", Type: "string", Tier: config.TierLive, Default: "", Description: "Origin HLS manifest the stitcher fetches and rewrites (the publisher's content playlist, with #EXT-X-CUE-OUT/CUE-IN ad-break markers). Empty (default) uses the built-in sample manifest; set it (or pass ?origin=<url>) to stitch a real origin. The request's ?origin= param overrides this.", Service: constants.ServiceSSAI, Since: "v1.6"},
	{Key: "ssai.transcoder_url", Type: "string", Tier: config.TierStatic, Default: routes.DefaultTranscoderURL, Description: "Runtime ad-conditioning service. On a break the stitcher asks it (cache-only) for the winning ad's segments, byte-compatible with the content; on a miss it warms the conditioner async and slates the break with the whole-file fallback. Empty disables conditioning (whole-file fallback only).", Service: constants.ServiceSSAI, Since: "v1.6"},
}
