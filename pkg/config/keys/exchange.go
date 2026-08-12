package keys

import (
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

var exchangeSet = config.NewKeySet(constants.ServiceExchange)

// ExchangeSchema is the Exchange schema — passed to config.Setup at boot.
func ExchangeSchema() []config.SchemaEntry { return exchangeSet.Entries() }

// Exchange holds the Exchange config keys.
var Exchange = struct {
	Channel                config.StringKey
	BidTimeout             config.DurationKey
	DSPEndpoints           config.StringKey
	NATSURL                config.StringKey
	MaxRetries             config.IntKey
	WinLossEnabled         config.BoolKey
	EmitDSPCallEvents      config.BoolKey
	DSPCallSampleRatio     config.FloatKey
	RoutingWarmstart       config.BoolKey
	RoutingReseedInterval  config.DurationKey
	RoutingEnabled         config.BoolKey
	RoutingMinCalls        config.IntKey
	RoutingMinBidRate      config.FloatKey
	RoutingMaxTimeoutRate  config.FloatKey
	RoutingExplorePct      config.FloatKey
	RoutingNeverSkip       config.StringKey
	RoutingLatencySoft     config.DurationKey
	RoutingLatencyHard     config.DurationKey
	RoutingRecencyWindow   config.IntKey
	ReportingURL           config.StringKey
	WarmDealsPollInterval  config.DurationKey
	AdsTxtEnforcement      config.StringKey
	AdsTxtSellerDomain     config.StringKey
	AdsTxtSellerID         config.StringKey
	WarmAdsTxtPollInterval config.DurationKey
	PrebidMinBidFloor      config.FloatKey
	RetailMinRelevance     config.FloatKey
	PrebidEnabled          config.BoolKey
	SchainEnforcement      config.StringKey
	SchainAppendNode       config.BoolKey
	AdCertSignKey          config.StringKey
	IdentityObserveEnabled config.BoolKey
	// PartnerRegistryEnabled merges 'active' DSP partners from the onboarding
	// registry (#112) into the auction fan-out; PartnerRefreshInterval is the warm
	// reload cadence (hot-path-safe — the auction reads an in-process snapshot).
	PartnerRegistryEnabled config.BoolKey
	PartnerRefreshInterval config.DurationKey

	// URL/Port are env/manifest territory by design — Raw, not in the schema.
	URL      config.StringKey
	Port     config.StringKey
	GRPCPort config.StringKey
}{
	Channel:                exchangeSet.String("exchange.channel", "all", config.TierLive, "Restricts which inventory channels this exchange instance auctions for (all, display, video, ctv, native, audio). Lets you stand up channel-dedicated instances without changing code.", config.Since("v1.0")),
	BidTimeout:             exchangeSet.Duration("exchange.bid_timeout", "500ms", config.TierLive, "Maximum time the exchange will wait for DSP bids before closing the auction. OpenRTB industry default is 100ms; we run 500ms locally because each DSP is a single Go process and concurrent load slows it.", config.Since("v1.0")),
	DSPEndpoints:           exchangeSet.String("exchange.dsp_endpoints", "http://localhost:8082,http://localhost:8089,http://localhost:8090", config.TierLive, "Comma-separated DSP base URLs the exchange fans bid requests out to. Smart routing further filters this list per auction. Live-tunable so e2e tests and ops can swap in test stubs or remove broken endpoints without restarting the exchange.", config.Since("v1.0")),
	NATSURL:                exchangeSet.String("exchange.nats_url", "nats://localhost:4222", config.TierStatic, "NATS JetStream URL for publishing AuctionWinEvent and AuctionCompleteEvent.", config.Since("v1.0")),
	MaxRetries:             exchangeSet.Int("exchange.max_retries", "30", config.TierStatic, "How many times the exchange retries binding its HTTP port on startup before exiting. Helps in Tilt where ports may still be in TIME_WAIT after a restart.", config.Since("v1.0")),
	WinLossEnabled:         exchangeSet.Bool("exchange.win_loss_enabled", "true", config.TierLive, "Send win/loss notifications back to DSPs after auction completes. Disable to silence them in load tests or when investigating DSP-side feedback loops.", config.Since("v1.0")),
	EmitDSPCallEvents:      exchangeSet.Bool("exchange.emit_dsp_call_events", "true", config.TierLive, "Publish one adtech.optimise.dsp_call event per DSP fan-out call (bid received?, price, latency, timeout) so routing behaviour is analysable in ClickHouse/Parquet and the SmartRouter can warm-start from it (ADR 0003). Fire-and-forget — never affects the auction. Disable in load tests where the extra publish volume is unwanted.", config.Since("v1.4")),
	DSPCallSampleRatio:     exchangeSet.Float("exchange.dsp_call_sample_ratio", "1.0", config.TierLive, "Fraction of auctions (0.0-1.0) whose DSP-call events are emitted — a background-load throttle for high-QPS exchanges. Sampled per auction by a deterministic hash of trace_id, so either all or none of an auction's per-DSP events are emitted (keeps the win-rate join consistent). 1.0 = every auction; lower to reduce NATS volume while still learning routing stats. Only consulted when exchange.emit_dsp_call_events=true.", config.Since("v1.4")),
	RoutingWarmstart:       exchangeSet.Bool("exchange.routing_warmstart", "true", config.TierStatic, "On boot, seed the SmartRouter from reporting's dsp_calls history (GET exchange.reporting_url/debug/routing/stats) so routing survives a restart with real per-DSP stats instead of re-learning from cold (ADR 0003). Async + fail-open — never blocks boot.", config.Since("v1.4")),
	RoutingReseedInterval:  exchangeSet.Duration("exchange.routing_reseed_interval", "5s", config.TierLive, "How often each exchange replica re-seeds its SmartRouter from reporting's cluster-global dsp_calls aggregate. At N replicas each pod sees only ~1/N of the fan-out calls, so per-pod stats alone never cross exchange.routing_min_calls and skip decisions diverge per pod; the periodic re-seed converges every pod to the shared totals (win history stays pod-local — it isn't in dsp_calls). 0 disables (per-pod stats + boot warm-start only; re-checked every 30s so re-enabling is live too). Fail-open: an unreachable reporting just leaves local stats in place until the next tick.", config.Since("v1.19")),
	RoutingEnabled:         exchangeSet.Bool("exchange.routing_enabled", "true", config.TierLive, "Smart-routing kill-switch. false = fan out to every configured DSP, ignoring learned skip rules and ranking — reach for this when routing misbehaves or while onboarding a new DSP that must not be throttled during ramp-up. Stats keep recording either way, so re-enabling picks up where it left off.", config.Since("v1.19")),
	RoutingMinCalls:        exchangeSet.Int("exchange.routing_min_calls", "20", config.TierLive, "Per-(channel, DSP) sample size before the skip rules may act — below this a DSP is always called, however bad its stats look (thin evidence never excludes anyone). Raise for more cautious skipping on high-QPS exchanges; lower to react faster in low-traffic environments.", config.Since("v1.19")),
	RoutingMinBidRate:      exchangeSet.Float("exchange.routing_min_bid_rate", "0.05", config.TierLive, "Skip a DSP on a channel when its bid rate falls below this fraction (0.05 = 5%) after exchange.routing_min_calls samples. The core traffic-shaping rule: stop paying fan-out latency for demand that never bids on this inventory type.", config.Since("v1.19")),
	RoutingMaxTimeoutRate:  exchangeSet.Float("exchange.routing_max_timeout_rate", "0.5", config.TierLive, "Skip a DSP on a channel when its timeout rate exceeds this fraction (0.5 = 50%) after exchange.routing_min_calls samples — a chronically slow DSP holds every auction to the bid deadline.", config.Since("v1.19")),
	RoutingExplorePct:      exchangeSet.Float("exchange.routing_explore_pct", "1", config.TierLive, "Exploration probe: percentage (0-100) of auctions that still call a skip-filtered DSP, decided deterministically per (trace, DSP). Keeps a skipped DSP's stats flowing so one that recovers earns its way back in within minutes — without it a skip lasts until the next pod restart. 0 disables (sticky skips).", config.Since("v1.19")),
	RoutingNeverSkip:       exchangeSet.String("exchange.routing_never_skip", "", config.TierLive, "Comma-separated DSP endpoints the skip rules must NEVER exclude. Paste entries as they appear in exchange.dsp_endpoints — a ;notify= suffix is stripped for matching, so the full entry and the bare bid endpoint both work. Use for demand holding PG/PMP deals: deals are evaluated from returned bids, so routing out the deal-holder silently starves a contractual guarantee regardless of its open-market bid rate.", config.Since("v1.19")),
	RoutingLatencySoft:     exchangeSet.Duration("exchange.routing_latency_soft", "50ms", config.TierLive, "Ranking (not skipping): a DSP whose average response latency exceeds this gets its expected-value score multiplied by 0.8, so a fast bidder outranks a slow one of equal value.", config.Since("v1.19")),
	RoutingLatencyHard:     exchangeSet.Duration("exchange.routing_latency_hard", "80ms", config.TierLive, "Ranking (not skipping): above this average latency the score multiplier drops to 0.5. Pair with exchange.bid_timeout — a DSP near the timeout is barely worth calling even when it bids well.", config.Since("v1.19")),
	RoutingRecencyWindow:   exchangeSet.Int("exchange.routing_recency_window", "200", config.TierLive, "Rolling-stat horizon in calls per (channel, DSP): bid/timeout rates and latency are EWMAs with this effective window, so a DSP is judged on its recent self, not its lifetime record — a skipped DSP that recovers rehabilitates after ~window probed calls instead of never (cumulative averages made the ε-probe's second chance hollow after a long bad history). Below the window the stats are exact cumulative means, so warm-up behaviour is unchanged.", config.Since("v1.20")),
	ReportingURL:           exchangeSet.String("exchange.reporting_url", "http://localhost:8086", config.TierStatic, "Base URL of the reporting service, used only for the routing warm-start aggregate fetch on boot (exchange.routing_warmstart).", config.Since("v1.4")),
	WarmDealsPollInterval:  exchangeSet.Duration("cache.warm.deals.poll_interval", "30s", config.TierStatic, "How often the in-memory deal cache refreshes from Postgres. Affects how quickly newly activated PG/PMP deals start preempting open-market bids.", config.Since("v1.1")),
	AdsTxtEnforcement:      exchangeSet.String("exchange.adstxt_enforcement", "off", config.TierLive, "ads.txt seller-authorisation enforcement before fan-out: 'off' (no check), 'warn' (log unauthorised publishers but allow), or 'strict' (no-bid requests from publishers whose ads.txt doesn't list us). Off by default; needs adstxt_seller_domain/id set and the ads_txt_cache populated (cmd/adstxt) to be meaningful.", config.Since("v1.3")),
	AdsTxtSellerDomain:     exchangeSet.String("exchange.adstxt_seller_domain", "", config.TierLive, "Our seller domain as it appears in publishers' ads.txt files (the first field of an authorising line). Consulted only when exchange.adstxt_enforcement != off. Live-tier so the gateway can surface the exact ads.txt line to publishers from the same shared value the exchange enforces on.", config.Since("v1.3")),
	AdsTxtSellerID:         exchangeSet.String("exchange.adstxt_seller_id", "", config.TierLive, "Our seller account ID as it appears in publishers' ads.txt files (the second field). Consulted only when exchange.adstxt_enforcement != off. Live-tier so the gateway can surface the exact ads.txt line to publishers from the same shared value the exchange enforces on.", config.Since("v1.3")),
	WarmAdsTxtPollInterval: exchangeSet.Duration("cache.warm.ads_txt.poll_interval", "300s", config.TierStatic, "How often the exchange refreshes the ads_txt_cache warm cache from Postgres. Long interval is fine — ads.txt files change rarely and the cmd/adstxt crawler is the slow-moving source of truth.", config.Since("v1.3")),
	PrebidMinBidFloor:      exchangeSet.Float("prebid.min_bid_floor", "0", config.TierLive, "Platform-wide minimum CPM floor applied to inbound Prebid Server bid requests. Effective floor per impression = max(inbound bidfloor, this value). Raise to enforce a global quality floor on Prebid demand.", config.Since("v1.3")),
	RetailMinRelevance:     exchangeSet.Float("exchange.retail_min_relevance", "0", config.TierLive, "Retail-media eligibility floor (0-1): a sponsored product whose relevance to the shopper's browsed categories scores below this doesn't show, however high its bid. 0 = no floor (rank everything by relevance × bid).", config.Since("v1.20")),
	PrebidEnabled:          exchangeSet.Bool("prebid.enabled", "true", config.TierLive, "Toggle the inbound Prebid bidder endpoint. Disable to stop accepting external Prebid Server traffic without restarting the exchange.", config.Since("v1.3")),
	SchainEnforcement:      exchangeSet.String("exchange.schain_enforcement", "warn", config.TierLive, "SupplyChain (schain) validation before fan-out: 'off' (no check), 'warn' (log requests with a missing/malformed schain but allow), or 'strict' (no-bid them). Default 'warn' because the SSP only emits schain once ssp.seller_domain is set — start in warn, ratchet to strict once every source populates it.", config.Since("v1.4")),
	SchainAppendNode:       exchangeSet.Bool("exchange.schain_append_node", "false", config.TierLive, "When true, append this exchange as an additional schain node using adstxt_seller_domain/id. Only correct when the exchange is a distinct reselling entity from the SSP — with the single-platform default (SSP already emits the platform node) leave this off to avoid inflating the chain.", config.Since("v1.4")),
	AdCertSignKey:          exchangeSet.String("exchange.adcert_sign_key", "", config.TierSecret, "ads.cert Ed25519 private key (base64, 64-byte seed+public form) used to sign outbound bid requests. Empty disables signing. DSPs verify with the matching public key (dsp.adcert_verify_key).", config.Since("v1.4")),
	IdentityObserveEnabled: exchangeSet.Bool("exchange.identity_observe_enabled", "false", config.TierStatic, "Publish identity signals from inbound Prebid bid requests (external demand our own SSP never saw) to the identity-consumer, which builds identity_graph edges. Fire-and-forget, off the hot path. Off by default; needs NATS.", config.Since("v1.4")),
	PartnerRegistryEnabled: exchangeSet.Bool("exchange.partner_registry_enabled", "true", config.TierLive, "Merge 'active' DSP partners from the onboarding registry (#112) into the auction fan-out, in addition to exchange.dsp_endpoints. The exchange warm-loads active partners from Postgres in the background; the auction reads an in-process snapshot (no per-auction query). ON by default: 'active' is the deliberate per-partner approval gate (register→sandbox→certify→active), so an approved partner receives live bid traffic — this flag is the platform-level kill-switch, not per-partner friction. Set false to instantly stop ALL partner traffic (incident lever).", config.Since("v1.21")),
	PartnerRefreshInterval: exchangeSet.Duration("exchange.partner_refresh_interval", "30s", config.TierStatic, "How often the exchange reloads the 'active' DSP-partner endpoint set from Postgres into its warm cache. Only consulted when exchange.partner_registry_enabled is on.", config.Since("v1.21")),
	URL:                    config.RawString("exchange.url", routes.DefaultExchangeURL),
	Port:                   config.RawString("exchange.port", routes.PortExchange),
	GRPCPort:               config.RawString("exchange.grpc_port", routes.PortExchangeGRPC),
}
