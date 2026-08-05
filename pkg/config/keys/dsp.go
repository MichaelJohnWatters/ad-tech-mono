package keys

import (
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

var dspSet = config.NewKeySet(constants.ServiceDSP)

// DSPSchema is the DSP schema — passed to config.Setup at boot.
func DSPSchema() []config.SchemaEntry { return dspSet.Entries() }

// DSP holds the DSP config keys.
var DSP = struct {
	Profile                            config.StringKey
	DailyBudgetDefault                 config.FloatKey
	MaxBidModifier                     config.FloatKey
	NoisePct                           config.FloatKey
	NoBidRate                          config.FloatKey
	BudgetResetInterval                config.DurationKey
	WarmCampaignsPollInterval          config.DurationKey
	SpendReconcileEnabled              config.BoolKey
	WarmOptOutsPollInterval            config.DurationKey
	AdCertVerifyKey                    config.StringKey
	AdCertEnforcement                  config.StringKey
	AdCertMaxAge                       config.DurationKey
	AdCertKeyURL                       config.StringKey
	AdCertKeyRefresh                   config.DurationKey
	IdentityResolutionEnabled          config.BoolKey
	IdentityMaxLinked                  config.IntKey
	IdentityPreloadInterval            config.DurationKey
	IdentityMaxDepth                   config.IntKey
	IdentityMinConfidence              config.FloatKey
	WarmAdvertiserBalancesPollInterval config.DurationKey
	BalanceGateEnabled                 config.BoolKey
	BidCacheRefreshInterval            config.DurationKey
	ResponseDelay                      config.DurationKey

	// URL/Port are env/manifest territory by design — Raw, not in the schema.
	URL      config.StringKey
	Port     config.StringKey
	GRPCPort config.StringKey

	NATSURL config.StringKey
}{
	Profile:                            dspSet.String("dsp.profile", "internal", config.TierStatic, "Which DSP identity this pod assumes (internal, competitor1, competitor2). Decides which dsps table row supplies noise_pct and no_bid_rate defaults.", config.Since("v1.0")),
	DailyBudgetDefault:                 dspSet.Float("dsp.daily_budget_default", "1000", config.TierLive, "Daily spend ceiling (USD) applied to a new campaign when the operator hasn't picked one. Per-campaign overrides win once a campaign is created.", config.Since("v1.0")),
	MaxBidModifier:                     dspSet.Float("dsp.max_bid_modifier", "200", config.TierLive, "Safety rail: maximum percentage a bid modifier can multiply a base bid (200 = 2x). Stops a runaway targeting rule from blowing through budget.", config.Since("v1.0")),
	NoisePct:                           dspSet.Float("dsp.noise_pct", "0", config.TierLive, "Adds ±N% random jitter to every bid (e.g. 30 = ±30%). 0 = exact bids. Used to simulate market noise in the realism profiles; in prod leave at 0.", config.Since("v1.1")),
	NoBidRate:                          dspSet.Float("dsp.no_bid_rate", "0", config.TierLive, "Probability (0-1) that the DSP randomly returns no_bid even when a campaign matches. Used by the competitor profiles to mimic flaky DSPs; leave at 0 for the real one.", config.Since("v1.1")),
	BudgetResetInterval:                dspSet.Duration("dsp.budget_reset_interval", "24h", config.TierLive, "How long Redis keeps a campaign's daily-spend counter before it expires back to zero. Effectively the rolling budget window length.", config.Since("v1.1")),
	WarmCampaignsPollInterval:          dspSet.Duration("cache.warm.campaigns.poll_interval", "30s", config.TierStatic, "How often the in-memory campaign cache refreshes from Postgres. Lower = faster pickup of campaign edits, higher = less DB load.", config.Since("v1.1")),
	SpendReconcileEnabled:              dspSet.Bool("dsp.spend_reconcile_enabled", "true", config.TierLive, "Reconcile campaign pacing counters to the billing engine's committed-spend snapshots (adtech.billing.campaign_spend_snapshot from reporting). Corrects the local win-notice decrement's over-count (phantom wins, full clearing price on CPC/CPA). Disable to pace purely on local win notices.", config.Since("v1.5")),
	WarmOptOutsPollInterval:            dspSet.Duration("cache.warm.opt_outs.poll_interval", "30s", config.TierStatic, "How often the user opt-out registry cache (consent enforcement on the bid path) refreshes from Postgres. Lower = faster pickup of new opt-outs, higher = less DB load. NATS invalidate on adtech.cache.invalidate.opt-outs propagates changes sub-second regardless.", config.Since("v1.3")),
	AdCertVerifyKey:                    dspSet.String("dsp.adcert_verify_key", "", config.TierStatic, "The exchange's ads.cert Ed25519 public key (base64) used to verify signed bid requests. Empty disables verification. Pairs with exchange.adcert_sign_key.", config.Since("v1.4")),
	AdCertEnforcement:                  dspSet.String("dsp.adcert_enforcement", "off", config.TierLive, "ads.cert signed-bid-request verification: 'off' (no check), 'warn' (bid but log requests with a missing/invalid signature), or 'strict' (no-bid them). Needs dsp.adcert_verify_key set and the exchange signing. Off by default.", config.Since("v1.4")),
	AdCertMaxAge:                       dspSet.Duration("dsp.adcert_max_age", "5m", config.TierLive, "Replay-protection window for ads.cert signed bid requests: a request whose signed timestamp is older (or further in the future) than this is treated as stale and fails verification. 0 disables the freshness check. Only consulted when adcert_enforcement != off.", config.Since("v1.4")),
	AdCertKeyURL:                       dspSet.String("dsp.adcert_key_url", "", config.TierStatic, "URL of the exchange's ads.cert public-key endpoint (/v1/adcert/key). When set, the DSP fetches + periodically refreshes the verification key from here instead of dsp.adcert_verify_key, so key rotations propagate without a restart. Falls back to the static key until the first fetch succeeds.", config.Since("v1.4")),
	AdCertKeyRefresh:                   dspSet.Duration("dsp.adcert_key_refresh", "5m", config.TierStatic, "How often the DSP re-fetches the exchange's ads.cert public key when dsp.adcert_key_url is set. A retained last-good key covers transient fetch failures.", config.Since("v1.4")),
	IdentityResolutionEnabled:          dspSet.Bool("dsp.identity_resolution_enabled", "false", config.TierLive, "When true, the DSP expands a user (User.id / UID2) via the identity_graph before the private-segment lookup, so segments attached to a linked identifier (another device / publisher / a UID2 CRM match) also apply. Adds a Postgres lookup to the bid path, so it's opt-in; degrades gracefully under the 25ms segment-lookup deadline.", config.Since("v1.4")),
	IdentityMaxLinked:                  dspSet.Int("dsp.identity_max_linked", "10", config.TierStatic, "Cap on how many identity-graph-linked ids the DSP folds into the private-segment lookup per bid (bounds the extra work when identity_resolution_enabled).", config.Since("v1.4")),
	IdentityPreloadInterval:            dspSet.Duration("dsp.identity_preload_interval", "5m", config.TierStatic, "How often the DSP reloads the whole identity graph into its in-memory snapshot. Bid-path resolution reads that snapshot (lock-free, no Postgres), so this is the only thing that touches the DB — keeps identity resolution QPS-safe. Each reload rebuilds the map, so a longer interval also cuts allocation churn; links change slowly, so 5m is a good default.", config.Since("v1.4")),
	IdentityMaxDepth:                   dspSet.Int("dsp.identity_max_depth", "3", config.TierStatic, "How many hops the DSP follows through the identity graph when resolving a user. 1 = direct links only; higher folds in the transitive closure (linked-of-linked), so e.g. uid2→email→device all resolve together. Bounds the per-bid BFS work.", config.Since("v1.4")),
	IdentityMinConfidence:              dspSet.Float("dsp.identity_min_confidence", "0", config.TierStatic, "Minimum edge confidence the DSP will traverse when resolving identity (0 = all edges). Raise (e.g. 0.8) to exclude low-confidence probabilistic links and only expand across deterministic ones.", config.Since("v1.4")),
	WarmAdvertiserBalancesPollInterval: dspSet.Duration("cache.warm.advertiser_balances.poll_interval", "30s", config.TierLive, "DSP balance warm-cache refresh. NATS invalidates (topup/drawdown) make this the fallback bound on balance staleness.", config.Since("v1.2")),
	BalanceGateEnabled:                 dspSet.Bool("dsp.balance_gate_enabled", "true", config.TierLive, "Gate bidding on the advertiser prepay balance (no funds -> no bid). Rollout escape hatch; disabling reverts to daily-budget-only enforcement.", config.Since("v1.2")),
	BidCacheRefreshInterval:            dspSet.Duration("dsp.bid_cache_refresh_interval", "1s", config.TierLive, "How often the background refresher bulk-MGETs the budget spend counters and balance draw-down mirrors into process memory. The bid loop reads ONLY these in-process copies (never Redis), so this bounds cross-pod staleness of budget pacing and the prepay gate.", config.Since("v1.11")),
	ResponseDelay:                      dspSet.Duration("dsp.response_delay", "0s", config.TierLive, "Artificial delay (jittered ±25%) added before handling every bid request — makes this pod a deliberately SLOW market participant. Powers the slowpoke scenario DSP (competitor1): set near/above exchange.bid_timeout so its timeout rate crosses the SmartRouter skip threshold every run; zero it live to watch the router rehabilitate the DSP via the ε-probe + recency window. 0 (default) disables.", config.Since("v1.20")),
	URL:                                config.RawString("dsp.url", routes.DefaultDSPURL),
	Port:                               config.RawString("dsp.port", routes.PortDSP),
	GRPCPort:                           config.RawString("dsp.grpc_port", routes.PortDSPGRPC),
	NATSURL:                            config.RawString("dsp.nats_url", routes.DefaultNATSURL),
}
