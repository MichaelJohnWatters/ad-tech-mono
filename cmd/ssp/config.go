package main

import (
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
)

// sspSchema is the SSP's owned config keys. Passed to config.Setup at boot;
// the pod writes the full schema (these + platform defaults) into its
// service_registry row.
var sspSchema = []config.SchemaEntry{
	{Key: "ssp.exchange_url", Type: "string", Tier: config.TierStatic, Default: "http://localhost:8081", Description: "Where the SSP sends bid requests. Points at the exchange; change to redirect SSP traffic to a different exchange instance.", Service: constants.ServiceSSP, Since: "v1.0"},
	{Key: "ssp.adserver_url", Type: "string", Tier: config.TierStatic, Default: "http://localhost:8085", Description: "Where /v1/ssp/serve fetches rendered creative HTML after an auction win. Points at the ad server.", Service: constants.ServiceSSP, Since: "v1.2"},
	{Key: "cache.warm.publishers.poll_interval", Type: "duration", Tier: config.TierStatic, Default: "30s", Description: "How often the in-memory publisher cache refreshes from Postgres. Controls how quickly newly enabled publishers start receiving bid requests.", Service: constants.ServiceSSP, Since: "v1.1"},
	{Key: "cache.warm.placements.poll_interval", Type: "duration", Tier: config.TierStatic, Default: "30s", Description: "How often the in-memory placement cache refreshes from Postgres. Affects how quickly floor-price and format edits take effect.", Service: constants.ServiceSSP, Since: "v1.1"},
	{Key: "ssp.seller_domain", Type: "string", Tier: config.TierStatic, Default: "", Description: "This platform's advertising-system domain, emitted as the asi of the first SupplyChain (schain) node on outbound bid requests. Should match the host serving sellers.json. Empty disables schain origination.", Service: constants.ServiceSSP, Since: "v1.3"},
	{Key: "ssp.seller_id", Type: "string", Tier: config.TierStatic, Default: "", Description: "Fallback seller id for the schain node when a placement has no publisher id. Normally the publisher's own seller id (matching sellers.json) is used instead.", Service: constants.ServiceSSP, Since: "v1.3"},
	{Key: "ssp.identity_observe_enabled", Type: "bool", Tier: config.TierStatic, Default: "false", Description: "Auto-build the identity graph from inbound requests: when a request carries 2+ identifiers (user_id, uid2, hashed_email, ifa) they're deterministically the same person, so edges linking them are written to identity_graph. Off the hot path (buffered + batched), best-effort. Off by default; needs database.url set.", Service: constants.ServiceSSP, Since: "v1.4"},
	{Key: "ssp.identity_flush_interval", Type: "duration", Tier: config.TierStatic, Default: "10s", Description: "How often the identity observer flushes its batch of newly-seen edges to Postgres. Only consulted when ssp.identity_observe_enabled.", Service: constants.ServiceSSP, Since: "v1.4"},
	{Key: "ssp.identity_seen_cap", Type: "int", Tier: config.TierStatic, Default: "100000", Description: "Max size of the identity observer's in-memory dedup set (bounds memory). When exceeded it resets — re-writing an edge later is idempotent, so this is safe. Only consulted when ssp.identity_observe_enabled.", Service: constants.ServiceSSP, Since: "v1.4"},
}
