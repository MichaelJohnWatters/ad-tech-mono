package keys

import (
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

var transcoderSet = config.NewKeySet(constants.ServiceTranscoder)

// TranscoderSchema is the Transcoder schema — passed to config.Setup at boot.
func TranscoderSchema() []config.SchemaEntry { return transcoderSet.Entries() }

// Transcoder holds the Transcoder config keys.
var Transcoder = struct {
	Port          config.StringKey
	Bucket        config.StringKey
	Prefix        config.StringKey
	PublicBase    config.StringKey
	FfmpegTimeout config.DurationKey

	// URL/Port are env/manifest territory by design — Raw, not in the schema.
	URL config.StringKey
}{
	Port:          transcoderSet.String("transcoder.port", routes.PortTranscoder, config.TierStatic, "Port the transcoder listens on.", config.Since("v1.6")),
	Bucket:        transcoderSet.String("transcoder.bucket", "adtech-creatives", config.TierStatic, "Object-store bucket for conditioned ad segments (public-read, same as creatives).", config.Since("v1.6")),
	Prefix:        transcoderSet.String("transcoder.prefix", "ssai/cond", config.TierStatic, "Object-key prefix for conditioned ad segments: {prefix}/{creative}/{profileHash}/.", config.Since("v1.6")),
	PublicBase:    transcoderSet.String("transcoder.public_base", "http://localhost:8080/v1/creatives", config.TierStatic, "Browser-reachable base URL for conditioned segments (the gateway /v1/creatives proxy). Segment URLs in the stitched manifest use this.", config.Since("v1.6")),
	FfmpegTimeout: transcoderSet.Duration("transcoder.ffmpeg_timeout", "3m", config.TierLive, "Per-conditioning ffmpeg wall-clock timeout.", config.Since("v1.6")),
	URL:           config.RawString("transcoder.url", routes.DefaultTranscoderURL),
}
