package main

import (
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// transcoderSchema — runtime ad-conditioning service config.
var transcoderSchema = []config.SchemaEntry{
	{Key: "transcoder.port", Type: "string", Tier: config.TierStatic, Default: routes.PortTranscoder, Description: "Port the transcoder listens on.", Service: constants.ServiceTranscoder, Since: "v1.6"},
	{Key: "transcoder.bucket", Type: "string", Tier: config.TierStatic, Default: "adtech-creatives", Description: "Object-store bucket for conditioned ad segments (public-read, same as creatives).", Service: constants.ServiceTranscoder, Since: "v1.6"},
	{Key: "transcoder.prefix", Type: "string", Tier: config.TierStatic, Default: "ssai/cond", Description: "Object-key prefix for conditioned ad segments: {prefix}/{creative}/{profileHash}/.", Service: constants.ServiceTranscoder, Since: "v1.6"},
	{Key: "transcoder.public_base", Type: "string", Tier: config.TierStatic, Default: "http://localhost:8080/v1/creatives", Description: "Browser-reachable base URL for conditioned segments (the gateway /v1/creatives proxy). Segment URLs in the stitched manifest use this.", Service: constants.ServiceTranscoder, Since: "v1.6"},
	{Key: "transcoder.ffmpeg_timeout", Type: "duration", Tier: config.TierLive, Default: "3m", Description: "Per-conditioning ffmpeg wall-clock timeout.", Service: constants.ServiceTranscoder, Since: "v1.6"},
}
