package keys

import (
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

var sspSet = config.NewKeySet(constants.ServiceSSP)

// SSPSchema is the SSP schema — passed to config.Setup at boot.
func SSPSchema() []config.SchemaEntry { return sspSet.Entries() }

// SSP holds the SSP config keys.
var SSP = struct {
	ExchangeURL                config.StringKey
	AdserverURL                config.StringKey
	WarmPublishersPollInterval config.DurationKey
	WarmPlacementsPollInterval config.DurationKey
	SellerDomain               config.StringKey
	SellerID                   config.StringKey
	IdentityObserveEnabled     config.BoolKey
	BehaviourObserveEnabled    config.BoolKey
	HouseholdEnabled           config.BoolKey
	HouseholdSalt              config.StringKey

	// URL/Port are env/manifest territory by design — Raw, not in the schema.
	URL  config.StringKey
	Port config.StringKey

	NATSURL config.StringKey
}{
	ExchangeURL:                sspSet.String("ssp.exchange_url", "http://localhost:8081", config.TierStatic, "Where the SSP sends bid requests. Points at the exchange; change to redirect SSP traffic to a different exchange instance.", config.Since("v1.0")),
	AdserverURL:                sspSet.String("ssp.adserver_url", "http://localhost:8085", config.TierStatic, "Where /v1/ssp/serve fetches rendered creative HTML after an auction win. Points at the ad server.", config.Since("v1.2")),
	WarmPublishersPollInterval: sspSet.Duration("cache.warm.publishers.poll_interval", "30s", config.TierStatic, "How often the in-memory publisher cache refreshes from Postgres. Controls how quickly newly enabled publishers start receiving bid requests.", config.Since("v1.1")),
	WarmPlacementsPollInterval: sspSet.Duration("cache.warm.placements.poll_interval", "30s", config.TierStatic, "How often the in-memory placement cache refreshes from Postgres. Affects how quickly floor-price and format edits take effect.", config.Since("v1.1")),
	SellerDomain:               sspSet.String("ssp.seller_domain", "", config.TierStatic, "This platform's advertising-system domain, emitted as the asi of the first SupplyChain (schain) node on outbound bid requests. Should match the host serving sellers.json. Empty disables schain origination.", config.Since("v1.3")),
	SellerID:                   sspSet.String("ssp.seller_id", "", config.TierStatic, "Fallback seller id for the schain node when a placement has no publisher id. Normally the publisher's own seller id (matching sellers.json) is used instead.", config.Since("v1.3")),
	IdentityObserveEnabled:     sspSet.Bool("ssp.identity_observe_enabled", "false", config.TierStatic, "Publish per-request identity signals (user_id, uid2, hashed_email, ifa, IP+UA fingerprint) to the identity-consumer, which builds identity_graph edges. Fire-and-forget, off the hot path. Off by default; needs NATS. The batching/dedup/probabilistic knobs live on the identity-consumer, not here.", config.Since("v1.4")),
	BehaviourObserveEnabled:    sspSet.Bool("ssp.behaviour_observe_enabled", "true", config.TierLive, "Publish one consent-gated behavioural signal row (user/household key + placement + publisher + content categories, stamped at event time) per ad request to adtech.behaviour.observed, landed in the behaviour_signals Delta table. Rows are only published when the request's regulatory signals permit personalisation. Input to behavioural segmentation (cmd/profile-builder).", config.Since("v1.9")),
	HouseholdEnabled:           sspSet.Bool("ssp.household_enabled", "true", config.TierLive, "Derive a household id (salted hash of the client IP — the CTV household proxy) and carry it as a user.eids entry on outbound bid requests, so DSPs can target household-scoped audience segments. The DSP only USES it under the consent gate, same as user segments.", config.Since("v1.8")),
	HouseholdSalt:              sspSet.String("ssp.household_salt", "adtech-local-dev-household", config.TierSecret, "HMAC salt for household-id derivation (identity.HouseholdID). Must be identical across SSP pods and any offline deriver (seed, tests) or household ids won't line up. Rotate = every household id changes.", config.Since("v1.8")),
	URL:                        config.RawString("ssp.url", routes.DefaultSSPURL),
	Port:                       config.RawString("ssp.port", routes.PortSSP),
	NATSURL:                    config.RawString("ssp.nats_url", routes.DefaultNATSURL),
}
