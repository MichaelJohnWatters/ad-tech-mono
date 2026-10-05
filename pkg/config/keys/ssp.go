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
	TrustedProxyHops           config.IntKey
	IPOverrideAllowlist        config.StringKey
	RateLimitRPS               config.IntKey
	RateLimitBurst             config.IntKey
	RateLimitTrustedHops       config.IntKey
	RateLimitAllowlist         config.StringKey
	RateLimitDistributed       config.BoolKey
	ConsumeDisplayAdM          config.BoolKey

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
	ConsumeDisplayAdM:          sspSet.Bool("ssp.consume_display_adm", "true", config.TierLive, "Serve an external display winner's bid.adm HTML (OpenRTB §4.3) wrapped with the platform's signed beacons when the creative is unknown to the ad server, instead of the generic placeholder. Frequency caps still enforce (the adserver cap decision precedes creative resolution). Off = legacy behavior: external display adm ignored, placeholder serves. Live ops lever if a buggy external creative slips through.", config.Since("v2.1")),
	TrustedProxyHops:           sspSet.Int("ssp.trusted_proxy_hops", "0", config.TierLive, "Number of trusted reverse proxies in front of the SSP, for END-USER IP resolution (household-id derivation, identity fingerprint). The client IP is taken this many entries from the RIGHT of X-Forwarded-For — the entries a trusted proxy appended — so a viewer cannot mint a fresh household per request by forging (prepending) XFF values. 0 (default) = the ingress is the only trusted hop: use the rightmost XFF entry. Set to 1 when a CDN sits in front of the ingress. Keep in step with ssp.ratelimit_trusted_proxy_hops (same topology, separate knob so a rate-limit tune can't silently move every household id).", config.Since("v2.1")),
	IPOverrideAllowlist:        sspSet.String("ssp.ip_override_allowlist", DefaultRateLimitAllowlist, config.TierLive, "Comma-separated CIDRs/IPs of CALLERS whose explicit ?ip= override is honoured for end-user IP resolution (household id, identity fingerprint). Server-side callers that legitimately forward the device IP — SSAI, server-side publisher tags, the simulator/e2e harness — sit in private ranges locally, hence the private-range default. A caller OUTSIDE the list (any public browser) has its ?ip= ignored: an unauthenticated override would let one device rotate households at will and bypass household frequency caps. Add publisher server egress ranges in prod; empty = never honour ?ip=.", config.Since("v2.1")),
	RateLimitRPS:               sspSet.Int("ssp.ratelimit_rps", "100", config.TierLive, "Per-client-IP HTTP request rate limit (requests/second) on the public SSP endpoints (bid-request intake / serve). ON by default (100/s per IP, burst 200) — a generous floor against a single abusive IP that won't hurt CGNAT'd real users; the CDN/WAF is the real volumetric shield. 0 = disabled. Buckets are per-pod (× replicas). Allowlisted IPs (ratelimit_allowlist — all private ranges by default, so load tests / the simulator bypass) + infra paths + CORS preflight are never limited.", config.Since("v1.16")),
	RateLimitBurst:             sspSet.Int("ssp.ratelimit_burst", "200", config.TierLive, "Token-bucket burst for ssp.ratelimit_rps — max requests in an instantaneous spike before the per-second rate applies. 0 = default to the rps value. Only meaningful when ratelimit_rps > 0.", config.Since("v1.16")),
	RateLimitTrustedHops:       sspSet.Int("ssp.ratelimit_trusted_proxy_hops", "0", config.TierLive, "Number of trusted reverse proxies in front of the SSP (your ingress, plus any CDN). The rate-limit client IP is taken this many entries from the RIGHT of X-Forwarded-For — the entries a trusted proxy appended — so a client cannot evade the limit by forging (prepending) X-Forwarded-For values. 0 (default) = the ingress is the only trusted hop: use the rightmost XFF entry. Set to 1 when a CDN sits in front of the ingress.", config.Since("v1.17")),
	RateLimitAllowlist:         sspSet.String("ssp.ratelimit_allowlist", DefaultRateLimitAllowlist, config.TierLive, "Comma-separated CIDRs/IPs that BYPASS the SSP rate limit. Defaults to loopback + private/link-local ranges so internal + local traffic is never throttled. Only consulted when ssp.ratelimit_rps > 0 (off by default — bid-request intake is high-volume and better shielded at the CDN/WAF).", config.Since("v1.17")),
	RateLimitDistributed:       sspSet.Bool("ssp.ratelimit_distributed", "false", config.TierLive, "Enforce ssp.ratelimit_rps CLUSTER-WIDE via a shared Redis fixed-window counter instead of per-pod in-process buckets. Costs one Redis op per limited request; fail-open if Redis is unreachable. Default false = per-pod. Only meaningful when ratelimit_rps > 0.", config.Since("v2.0")),
	URL:                        config.RawString("ssp.url", routes.DefaultSSPURL),
	Port:                       config.RawString("ssp.port", routes.PortSSP),
	NATSURL:                    config.RawString("ssp.nats_url", routes.DefaultNATSURL),
}
