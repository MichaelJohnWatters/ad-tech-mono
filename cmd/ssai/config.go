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
	{Key: "ssai.segment_duration", Type: "float", Tier: config.TierLive, Default: "6", Description: "Target HLS segment duration (seconds) the stitcher slices an ad creative into when inserting it into the manifest.", Service: constants.ServiceSSAI, Since: "v1.5"},
	{Key: "ssai.ad_placement_id", Type: "string", Tier: config.TierLive, Default: "pl-sim-video", Description: "Placement id used for the per-break auction when the manifest request doesn't specify one.", Service: constants.ServiceSSAI, Since: "v1.5"},
}
