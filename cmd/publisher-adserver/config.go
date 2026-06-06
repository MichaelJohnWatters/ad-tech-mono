package main

import (
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
)

// publisherAdServerSchema is the per-service config schema. Mirrors the
// shape used by other services' config.go: live-tunable behaviour goes in
// the schema, infra (database URL, NATS URL, etc.) comes from the
// platform-default schema.
var publisherAdServerSchema = []config.SchemaEntry{
	{Key: "publisher_adserver.ssp_url", Type: "string", Tier: config.TierStatic, Default: "http://localhost:8084", Description: "Where the publisher ad server falls through to for programmatic auctions when no direct-sold line item wins.", Service: constants.ServicePublisherAdServer, Since: "v1.3"},
	{Key: "publisher_adserver.adserver_url", Type: "string", Tier: config.TierStatic, Default: "http://localhost:8085", Description: "Where to fetch rendered creative HTML when a direct-sold line item wins. Same ad server used by the SSP for programmatic.", Service: constants.ServicePublisherAdServer, Since: "v1.3"},
	{Key: "publisher_adserver.tracker_url", Type: "string", Tier: config.TierStatic, Default: "http://localhost:8083", Description: "Base URL for impression / click / viewability beacons injected into external Prebid bid HTML. MUST be browser-reachable — cluster DNS like 'tracker:8083' will not resolve from end-user browsers. In pod mode point at the gateway's /v1/t/* reverse proxy. Without this we record the Prebid win but downstream view + click signals fall through to the external bidder's own infra.", Service: constants.ServicePublisherAdServer, Since: "v1.4"},
	{Key: "cache.warm.publisher_line_items.poll_interval", Type: "duration", Tier: config.TierStatic, Default: "30s", Description: "How often the in-memory publisher line item cache refreshes from Postgres. Affects how quickly newly trafficked direct deals take effect.", Service: constants.ServicePublisherAdServer, Since: "v1.3"},
	{Key: "publisher_adserver.prebid_servers", Type: "string", Tier: config.TierLive, Default: "", Description: "Comma-separated external Prebid Server bidder endpoints to fan out to on programmatic fall-through, alongside our own SSP. Empty (default) = SSP only. Live-tunable so publishers can add/remove Prebid demand sources without a restart.", Service: constants.ServicePublisherAdServer, Since: "v1.3"},
	{Key: "publisher_adserver.prebid_timeout", Type: "duration", Tier: config.TierLive, Default: "500ms", Description: "Per-call timeout for outbound Prebid Server requests. Matches our inbound exchange.bid_timeout so callers can't make us slower than we'd accept ourselves.", Service: constants.ServicePublisherAdServer, Since: "v1.3"},
}
