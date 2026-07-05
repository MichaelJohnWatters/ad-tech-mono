package main

import (
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
)

// exchangeSchema is the exchange service's owned config keys. Passed into
// config.Setup at boot; the pod publishes the full schema (these entries +
// platform defaults) into its service_registry row. Adding a new
// auction-side knob = add an entry here.
var exchangeSchema = []config.SchemaEntry{
	{Key: "exchange.channel", Type: "string", Tier: config.TierLive, Default: "all", Description: "Restricts which inventory channels this exchange instance auctions for (all, display, video, ctv, native, audio). Lets you stand up channel-dedicated instances without changing code.", Service: constants.ServiceExchange, Since: "v1.0"},
	{Key: "exchange.bid_timeout", Type: "duration", Tier: config.TierLive, Default: "500ms", Description: "Maximum time the exchange will wait for DSP bids before closing the auction. OpenRTB industry default is 100ms; we run 500ms locally because each DSP is a single Go process and concurrent load slows it.", Service: constants.ServiceExchange, Since: "v1.0"},
	{Key: "exchange.dsp_endpoints", Type: "string", Tier: config.TierLive, Default: "http://localhost:8082,http://localhost:8089,http://localhost:8090", Description: "Comma-separated DSP base URLs the exchange fans bid requests out to. Smart routing further filters this list per auction. Live-tunable so e2e tests and ops can swap in test stubs or remove broken endpoints without restarting the exchange.", Service: constants.ServiceExchange, Since: "v1.0"},
	{Key: "exchange.nats_url", Type: "string", Tier: config.TierStatic, Default: "nats://localhost:4222", Description: "NATS JetStream URL for publishing AuctionWinEvent and AuctionCompleteEvent.", Service: constants.ServiceExchange, Since: "v1.0"},
	{Key: "exchange.max_retries", Type: "int", Tier: config.TierStatic, Default: "30", Description: "How many times the exchange retries binding its HTTP port on startup before exiting. Helps in Tilt where ports may still be in TIME_WAIT after a restart.", Service: constants.ServiceExchange, Since: "v1.0"},
	{Key: "exchange.win_loss_enabled", Type: "bool", Tier: config.TierLive, Default: "true", Description: "Send win/loss notifications back to DSPs after auction completes. Disable to silence them in load tests or when investigating DSP-side feedback loops.", Service: constants.ServiceExchange, Since: "v1.0"},
	{Key: "exchange.emit_dsp_call_events", Type: "bool", Tier: config.TierLive, Default: "true", Description: "Publish one adtech.optimise.dsp_call event per DSP fan-out call (bid received?, price, latency, timeout) so routing behaviour is analysable in ClickHouse/Parquet and the SmartRouter can warm-start from it (ADR 0003). Fire-and-forget — never affects the auction. Disable in load tests where the extra publish volume is unwanted.", Service: constants.ServiceExchange, Since: "v1.4"},
	{Key: "exchange.dsp_call_sample_ratio", Type: "float", Tier: config.TierLive, Default: "1.0", Description: "Fraction of auctions (0.0-1.0) whose DSP-call events are emitted — a background-load throttle for high-QPS exchanges. Sampled per auction by a deterministic hash of trace_id, so either all or none of an auction's per-DSP events are emitted (keeps the win-rate join consistent). 1.0 = every auction; lower to reduce NATS volume while still learning routing stats. Only consulted when exchange.emit_dsp_call_events=true.", Service: constants.ServiceExchange, Since: "v1.4"},
	{Key: "exchange.routing_warmstart", Type: "bool", Tier: config.TierStatic, Default: "true", Description: "On boot, seed the SmartRouter from reporting's dsp_calls history (GET exchange.reporting_url/debug/routing/stats) so routing survives a restart with real per-DSP stats instead of re-learning from cold (ADR 0003). Async + fail-open — never blocks boot.", Service: constants.ServiceExchange, Since: "v1.4"},
	{Key: "exchange.reporting_url", Type: "string", Tier: config.TierStatic, Default: "http://localhost:8086", Description: "Base URL of the reporting service, used only for the routing warm-start aggregate fetch on boot (exchange.routing_warmstart).", Service: constants.ServiceExchange, Since: "v1.4"},
	{Key: "cache.warm.deals.poll_interval", Type: "duration", Tier: config.TierStatic, Default: "30s", Description: "How often the in-memory deal cache refreshes from Postgres. Affects how quickly newly activated PG/PMP deals start preempting open-market bids.", Service: constants.ServiceExchange, Since: "v1.1"},
	{Key: "exchange.adstxt_enforcement", Type: "string", Tier: config.TierLive, Default: "off", Description: "ads.txt seller-authorisation enforcement before fan-out: 'off' (no check), 'warn' (log unauthorised publishers but allow), or 'strict' (no-bid requests from publishers whose ads.txt doesn't list us). Off by default; needs adstxt_seller_domain/id set and the ads_txt_cache populated (cmd/adstxt) to be meaningful.", Service: constants.ServiceExchange, Since: "v1.3"},
	{Key: "exchange.adstxt_seller_domain", Type: "string", Tier: config.TierStatic, Default: "", Description: "Our seller domain as it appears in publishers' ads.txt files (the first field of an authorising line). Consulted only when exchange.adstxt_enforcement != off.", Service: constants.ServiceExchange, Since: "v1.3"},
	{Key: "exchange.adstxt_seller_id", Type: "string", Tier: config.TierStatic, Default: "", Description: "Our seller account ID as it appears in publishers' ads.txt files (the second field). Consulted only when exchange.adstxt_enforcement != off.", Service: constants.ServiceExchange, Since: "v1.3"},
	{Key: "cache.warm.ads_txt.poll_interval", Type: "duration", Tier: config.TierStatic, Default: "300s", Description: "How often the exchange refreshes the ads_txt_cache warm cache from Postgres. Long interval is fine — ads.txt files change rarely and the cmd/adstxt crawler is the slow-moving source of truth.", Service: constants.ServiceExchange, Since: "v1.3"},
	{Key: "prebid.min_bid_floor", Type: "float", Tier: config.TierLive, Default: "0", Description: "Platform-wide minimum CPM floor applied to inbound Prebid Server bid requests. Effective floor per impression = max(inbound bidfloor, this value). Raise to enforce a global quality floor on Prebid demand.", Service: constants.ServiceExchange, Since: "v1.3"},
	{Key: "prebid.enabled", Type: "bool", Tier: config.TierLive, Default: "true", Description: "Toggle the inbound Prebid bidder endpoint. Disable to stop accepting external Prebid Server traffic without restarting the exchange.", Service: constants.ServiceExchange, Since: "v1.3"},
	{Key: "exchange.schain_enforcement", Type: "string", Tier: config.TierLive, Default: "warn", Description: "SupplyChain (schain) validation before fan-out: 'off' (no check), 'warn' (log requests with a missing/malformed schain but allow), or 'strict' (no-bid them). Default 'warn' because the SSP only emits schain once ssp.seller_domain is set — start in warn, ratchet to strict once every source populates it.", Service: constants.ServiceExchange, Since: "v1.4"},
	{Key: "exchange.schain_append_node", Type: "bool", Tier: config.TierLive, Default: "false", Description: "When true, append this exchange as an additional schain node using adstxt_seller_domain/id. Only correct when the exchange is a distinct reselling entity from the SSP — with the single-platform default (SSP already emits the platform node) leave this off to avoid inflating the chain.", Service: constants.ServiceExchange, Since: "v1.4"},
}

// Knobs is the exchange's typed config accessor. See cmd/dsp/config.go for
// the pattern explanation.
type Knobs struct {
	cfg *config.Config

	// Live (consumed per-request in the auction handler — atomic swap so
	// UI edits land without a restart).
	BidTimeout *config.LiveDuration
}

// NewKnobs binds the live keys to the manager. Called once at boot.
func NewKnobs(sc *config.ServiceConfig) *Knobs {
	return &Knobs{
		cfg:        sc.Cfg,
		BidTimeout: config.NewLiveDuration(sc.Manager, sc.Cfg, "exchange.bid_timeout", 500*time.Millisecond),
	}
}

// Channel is the inventory-channel restriction for this exchange instance
// (all, display, video, ctv, native, audio). TierLive — read per auction.
func (k *Knobs) Channel() string { return k.cfg.Get("exchange.channel", constants.ChannelAll) }

// WinLossEnabled controls whether the exchange sends win/loss callbacks to
// DSPs. TierLive — read per auction.
func (k *Knobs) WinLossEnabled() bool { return k.cfg.GetBool("exchange.win_loss_enabled", true) }
