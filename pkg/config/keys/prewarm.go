package keys

import (
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

var prewarmSet = config.NewKeySet(constants.ServiceTranscoder)

// PrewarmSchema is the Prewarm schema — passed to config.Setup at boot.
func PrewarmSchema() []config.SchemaEntry {
	return append(prewarmSet.Entries(), transcodeSharedEntries()...)
}

// Prewarm holds the Prewarm config keys.
var Prewarm = struct {
	TranscoderURL config.StringKey
}{
	TranscoderURL: prewarmSet.String("prewarm.transcoder_url", routes.DefaultTranscoderURL, config.TierStatic, "Transcoder the prewarm job conditions creatives against.", config.Since("v1.6")),
}
