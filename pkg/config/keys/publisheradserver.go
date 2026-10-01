package keys

import (
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

var publisherAdServerSet = config.NewKeySet(constants.ServicePublisherAdServer)

// PublisherAdServerSchema is the PublisherAdServer schema — passed to config.Setup at boot.
func PublisherAdServerSchema() []config.SchemaEntry { return publisherAdServerSet.Entries() }

// PublisherAdServer holds the PublisherAdServer config keys.
var PublisherAdServer = struct {
	SSPURL                             config.StringKey
	AdserverURL                        config.StringKey
	TrackerURL                         config.StringKey
	WarmPublisherLineItemsPollInterval config.DurationKey
	PrebidServers                      config.StringKey
	PrebidTimeout                      config.DurationKey
	OmidVerificationURL                config.StringKey
	OmidVendor                         config.StringKey
	StubOnNobid                        config.BoolKey

	// URL/Port are env/manifest territory by design — Raw, not in the schema.
	URL  config.StringKey
	Port config.StringKey

	NATSURL   config.StringKey
	PublicURL config.StringKey

	PublicURLSecure config.StringKey
}{
	SSPURL:                             publisherAdServerSet.String("publisher_adserver.ssp_url", "http://localhost:8084", config.TierStatic, "Where the publisher ad server falls through to for programmatic auctions when no direct-sold line item wins.", config.Since("v1.3")),
	AdserverURL:                        publisherAdServerSet.String("publisher_adserver.adserver_url", "http://localhost:8085", config.TierStatic, "Where to fetch rendered creative HTML when a direct-sold line item wins. Same ad server used by the SSP for programmatic.", config.Since("v1.3")),
	TrackerURL:                         publisherAdServerSet.String("publisher_adserver.tracker_url", "http://localhost:8083", config.TierStatic, "Base URL for impression / click / viewability beacons injected into external Prebid bid HTML. MUST be browser-reachable — cluster DNS like 'tracker:8083' will not resolve from end-user browsers. In pod mode point at the gateway's /v1/t/* reverse proxy. Without this we record the Prebid win but downstream view + click signals fall through to the external bidder's own infra.", config.Since("v1.4")),
	WarmPublisherLineItemsPollInterval: publisherAdServerSet.Duration("cache.warm.publisher_line_items.poll_interval", "30s", config.TierStatic, "How often the in-memory publisher line item cache refreshes from Postgres. Affects how quickly newly trafficked direct deals take effect.", config.Since("v1.3")),
	PrebidServers:                      publisherAdServerSet.String("publisher_adserver.prebid_servers", "", config.TierLive, "Comma-separated external Prebid Server bidder endpoints to fan out to on programmatic fall-through, alongside our own SSP. Empty (default) = SSP only. Live-tunable so publishers can add/remove Prebid demand sources without a restart.", config.Since("v1.3")),
	PrebidTimeout:                      publisherAdServerSet.Duration("publisher_adserver.prebid_timeout", "500ms", config.TierLive, "Per-call timeout for outbound Prebid Server requests. Matches our inbound exchange.bid_timeout so callers can't make us slower than we'd accept ourselves.", config.Since("v1.3")),
	OmidVerificationURL:                publisherAdServerSet.String("publisher_adserver.omid_verification_url", "", config.TierLive, "Open Measurement (OMID) verification script URL to embed as <AdVerifications> in served VAST. Empty (default) = no AdVerifications element. Point at a measurement vendor's OM SDK verification JS to enable viewability/verification measurement on video ads.", config.Since("v1.4")),
	OmidVendor:                         publisherAdServerSet.String("publisher_adserver.omid_vendor", "ad-tech-mono-omid", config.TierLive, "Vendor key attribute for the OMID <Verification> element. Only used when publisher_adserver.omid_verification_url is set.", config.Since("v1.4")),
	StubOnNobid:                        publisherAdServerSet.Bool("publisher_adserver.stub_on_nobid", "false", config.TierLive, "When true, serve a canned demo (house) ad on a video/native/audio no-bid instead of an honest empty no-fill. OFF by default: the platform serves only real auctioned demand — a no-bid returns an empty VAST / 204 (no ad, not fake data). Enable only for a fully-empty dev environment that needs something renderable.", config.Since("v1.6")),
	URL:                                config.RawString("publisher_adserver.url", routes.DefaultPublisherAdServerURL),
	Port:                               config.RawString("publisher_adserver.port", routes.PortPublisherAdServer),
	NATSURL:                            config.RawString("publisher_adserver.nats_url", routes.DefaultNATSURL),
	PublicURL:                          config.RawString("publisher_adserver.public_url", "http://localhost:8080"),
	PublicURLSecure:                    publisherAdServerSet.String("publisher_adserver.public_url_secure", "https://gateway.adtech.local", config.TierStatic, "Browser-reachable HTTPS base (scheme+host) used for ad MEDIA and tracking BEACONS when the inbound serve request arrives over the HTTPS ingress (X-Forwarded-Proto: https, set by Traefik). Prevents mixed-content blocking on HTTPS demo publisher sites. Requests without that header (pub-simulator via the bridge, the e2e harness, CI) keep emitting publisher_adserver.tracker_url / the DB media host unchanged. Overridable per-environment via PUBLISHER_ADSERVER_PUBLIC_URL_SECURE.", config.Since("v1.7")),
}
