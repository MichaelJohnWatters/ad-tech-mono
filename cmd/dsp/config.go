package main

import (
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
)

// dspSchema is the DSP's owned config keys. Passed to config.Setup at boot;
// the pod writes the full schema (these entries + the platform-shared
// defaults from pkg/config.defaultSchema) into its service_registry row so
// the config-manager UI can render them. Adding a new DSP knob = add an
// entry here.
//
// Platform-shared keys (database.*, redis.*, otel.*, etc.) live in
// pkg/config.defaultSchema and are merged in automatically — don't repeat
// them here.
var dspSchema = []config.SchemaEntry{
	{Key: "dsp.profile", Type: "string", Tier: config.TierStatic, Default: "internal", Description: "Which DSP identity this pod assumes (internal, competitor1, competitor2). Decides which dsps table row supplies noise_pct and no_bid_rate defaults.", Service: constants.ServiceDSP, Since: "v1.0"},
	{Key: "dsp.daily_budget_default", Type: "float", Tier: config.TierLive, Default: "1000", Description: "Daily spend ceiling (USD) applied to a new campaign when the operator hasn't picked one. Per-campaign overrides win once a campaign is created.", Service: constants.ServiceDSP, Since: "v1.0"},
	{Key: "dsp.max_bid_modifier", Type: "float", Tier: config.TierLive, Default: "200", Description: "Safety rail: maximum percentage a bid modifier can multiply a base bid (200 = 2x). Stops a runaway targeting rule from blowing through budget.", Service: constants.ServiceDSP, Since: "v1.0"},
	{Key: "dsp.noise_pct", Type: "float", Tier: config.TierLive, Default: "0", Description: "Adds ±N% random jitter to every bid (e.g. 30 = ±30%). 0 = exact bids. Used to simulate market noise in the realism profiles; in prod leave at 0.", Service: constants.ServiceDSP, Since: "v1.1"},
	{Key: "dsp.no_bid_rate", Type: "float", Tier: config.TierLive, Default: "0", Description: "Probability (0-1) that the DSP randomly returns no_bid even when a campaign matches. Used by the competitor profiles to mimic flaky DSPs; leave at 0 for the real one.", Service: constants.ServiceDSP, Since: "v1.1"},
	{Key: "dsp.budget_reset_interval", Type: "duration", Tier: config.TierLive, Default: "24h", Description: "How long Redis keeps a campaign's daily-spend counter before it expires back to zero. Effectively the rolling budget window length.", Service: constants.ServiceDSP, Since: "v1.1"},
	{Key: "cache.warm.campaigns.poll_interval", Type: "duration", Tier: config.TierStatic, Default: "30s", Description: "How often the in-memory campaign cache refreshes from Postgres. Lower = faster pickup of campaign edits, higher = less DB load.", Service: constants.ServiceDSP, Since: "v1.1"},
	{Key: "cache.warm.opt_outs.poll_interval", Type: "duration", Tier: config.TierStatic, Default: "30s", Description: "How often the user opt-out registry cache (consent enforcement on the bid path) refreshes from Postgres. Lower = faster pickup of new opt-outs, higher = less DB load. NATS invalidate on adtech.cache.invalidate.opt-outs propagates changes sub-second regardless.", Service: constants.ServiceDSP, Since: "v1.3"},
	{Key: "dsp.adcert_verify_key", Type: "string", Tier: config.TierStatic, Default: "", Description: "The exchange's ads.cert Ed25519 public key (base64) used to verify signed bid requests. Empty disables verification. Pairs with exchange.adcert_sign_key.", Service: constants.ServiceDSP, Since: "v1.4"},
	{Key: "dsp.adcert_enforcement", Type: "string", Tier: config.TierLive, Default: "off", Description: "ads.cert signed-bid-request verification: 'off' (no check), 'warn' (bid but log requests with a missing/invalid signature), or 'strict' (no-bid them). Needs dsp.adcert_verify_key set and the exchange signing. Off by default.", Service: constants.ServiceDSP, Since: "v1.4"},
	{Key: "dsp.adcert_max_age", Type: "duration", Tier: config.TierLive, Default: "5m", Description: "Replay-protection window for ads.cert signed bid requests: a request whose signed timestamp is older (or further in the future) than this is treated as stale and fails verification. 0 disables the freshness check. Only consulted when adcert_enforcement != off.", Service: constants.ServiceDSP, Since: "v1.4"},
	{Key: "dsp.adcert_key_url", Type: "string", Tier: config.TierStatic, Default: "", Description: "URL of the exchange's ads.cert public-key endpoint (/v1/adcert/key). When set, the DSP fetches + periodically refreshes the verification key from here instead of dsp.adcert_verify_key, so key rotations propagate without a restart. Falls back to the static key until the first fetch succeeds.", Service: constants.ServiceDSP, Since: "v1.4"},
	{Key: "dsp.adcert_key_refresh", Type: "duration", Tier: config.TierStatic, Default: "5m", Description: "How often the DSP re-fetches the exchange's ads.cert public key when dsp.adcert_key_url is set. A retained last-good key covers transient fetch failures.", Service: constants.ServiceDSP, Since: "v1.4"},
	{Key: "dsp.identity_resolution_enabled", Type: "bool", Tier: config.TierLive, Default: "false", Description: "When true, the DSP expands a user (User.id / UID2) via the identity_graph before the private-segment lookup, so segments attached to a linked identifier (another device / publisher / a UID2 CRM match) also apply. Adds a Postgres lookup to the bid path, so it's opt-in; degrades gracefully under the 25ms segment-lookup deadline.", Service: constants.ServiceDSP, Since: "v1.4"},
	{Key: "dsp.identity_max_linked", Type: "int", Tier: config.TierStatic, Default: "10", Description: "Cap on how many identity-graph-linked ids the DSP folds into the private-segment lookup per bid (bounds the extra work when identity_resolution_enabled).", Service: constants.ServiceDSP, Since: "v1.4"},
	{Key: "dsp.identity_preload_interval", Type: "duration", Tier: config.TierStatic, Default: "5m", Description: "How often the DSP reloads the whole identity graph into its in-memory snapshot. Bid-path resolution reads that snapshot (lock-free, no Postgres), so this is the only thing that touches the DB — keeps identity resolution QPS-safe. Each reload rebuilds the map, so a longer interval also cuts allocation churn; links change slowly, so 5m is a good default.", Service: constants.ServiceDSP, Since: "v1.4"},
}

// Knobs is the DSP service's typed config accessor. Methods read live every
// call (scalars are cheap RLock + map lookup); Live* fields are pre-bound
// atomic.Pointer holders for keys whose value is consumed inside a
// constructed object (e.g. BudgetTracker holds a TTL — has to be a Live*
// so a UI edit actually takes effect on the next budget write).
//
// Defaults are duplicated with the schema above on purpose: the schema is
// the source of truth for the UI + validation, while these defaults are
// the fallback used when neither Postgres nor the env var is set. Both
// must agree — change them in tandem.
type Knobs struct {
	cfg *config.Config

	// Live (consumed inside a constructor; needs atomic swap on change)
	BudgetResetInterval *config.LiveDuration
}

// NewKnobs binds the live keys to the manager and returns the typed
// accessor. Called once at boot.
func NewKnobs(sc *config.ServiceConfig) *Knobs {
	return &Knobs{
		cfg:                 sc.Cfg,
		BudgetResetInterval: config.NewLiveDuration(sc.Manager, sc.Cfg, "dsp.budget_reset_interval", 24*time.Hour),
	}
}

// Profile returns the DSP profile name (internal, competitor1, competitor2).
// TierStatic — env var override at boot wins; UI edits don't apply.
func (k *Knobs) Profile() string { return k.cfg.Get("dsp.profile", "internal") }

// DailyBudgetDefault is the daily budget applied when a campaign doesn't
// set its own. TierLive — read on every campaign creation path.
func (k *Knobs) DailyBudgetDefault() float64 {
	return k.cfg.GetFloat("dsp.daily_budget_default", 1000)
}

// MaxBidModifier caps how much a targeting rule can multiply a base bid.
// TierLive — read at every bid evaluation.
func (k *Knobs) MaxBidModifier() float64 { return k.cfg.GetFloat("dsp.max_bid_modifier", 200) }

// NoisePct is the ±% jitter added to every bid. TierLive.
func (k *Knobs) NoisePct() float64 { return k.cfg.GetFloat("dsp.noise_pct", 0) }

// NoBidRate is the probability (0-1) of a random no-bid. TierLive.
func (k *Knobs) NoBidRate() float64 { return k.cfg.GetFloat("dsp.no_bid_rate", 0) }
