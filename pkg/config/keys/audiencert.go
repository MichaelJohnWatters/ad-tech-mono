package keys

import (
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

var audienceRTSet = config.NewKeySet(constants.ServiceAudienceRT)

// AudienceRTSchema is the audience-rt schema — passed to config.Setup at boot.
func AudienceRTSchema() []config.SchemaEntry { return audienceRTSet.Entries() }

// AudienceRT holds the audience-rt (real-time retargeting consumer) config keys.
var AudienceRT = struct {
	Port    config.StringKey
	NATSURL config.StringKey
}{
	Port:    audienceRTSet.String("audience_rt.port", routes.PortAudienceRT, config.TierStatic, "HTTP port for /healthz, /readyz, /metrics.", config.Since("v1.5")),
	NATSURL: audienceRTSet.String("audience_rt.nats_url", routes.DefaultNATSURL, config.TierStatic, "NATS JetStream URL. Without it no behaviour/conversion events are consumed and readiness fails.", config.Since("v1.5")),
}
