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
	Port            config.StringKey
	NATSURL         config.StringKey
	PurgeInterval   config.DurationKey
	HouseholdEnroll config.BoolKey
}{
	Port:            audienceRTSet.String("audience_rt.port", routes.PortAudienceRT, config.TierStatic, "HTTP port for /healthz, /readyz, /metrics.", config.Since("v1.5")),
	NATSURL:         audienceRTSet.String("audience_rt.nats_url", routes.DefaultNATSURL, config.TierStatic, "NATS JetStream URL. Without it no behaviour/conversion events are consumed and readiness fails.", config.Since("v1.5")),
	PurgeInterval:   audienceRTSet.Duration("audience_rt.purge_interval", "60s", config.TierStatic, "How often to physically delete expired retargeting members (audience_segment_members past their TTL). Read paths already exclude expired rows, so this is storage hygiene — a longer interval is fine.", config.Since("v1.5")),
	HouseholdEnroll: audienceRTSet.Bool("audience_rt.household_enroll", "true", config.TierLive, "When true, a retargeting site_visit that carries a household id (hh: salted-IP hash, derived by the tracker's rt pixel) enrolls the HOUSEHOLD alongside the visitor id — so anonymous guest carts are chaseable on any device in the home (incl. CTV) without an email bridge. Coarser than person-level retargeting: the whole household sees the chase. Same TTL as the visitor enrollment; purchase suppression currently releases only the converting id (person/household-level suppression is a recorded open decision).", config.Since("v2.2")),
}
