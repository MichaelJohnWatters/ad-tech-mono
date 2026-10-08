package keys

import (
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
)

// Audience holds the shared audience-segment cache knobs read by the DSP and
// SSP. Raw: never registered in any schema — promote to a KeySet if they
// should become UI-tunable.
var Audience = struct {
	PreloadInterval    config.DurationKey
	CacheTTL           config.DurationKey
	TaxonomyRefresh    config.DurationKey
	ChangelogPoll      config.DurationKey
	ChangelogReconcile config.DurationKey
	ChangelogLagWarn   config.DurationKey
	// L1 cache hit-rate probe (registered on BOTH the DSP and SSP schemas since
	// both read the per-bid audience lookup). Shadow-only: counts would-be L1
	// hits/misses, changes nothing. Separate handles per service because a key
	// is registered once per schema.
	L1ProbeEnabledDSP config.BoolKey
	L1ProbeEnabledSSP config.BoolKey
	L1ProbeTTLDSP     config.DurationKey
	L1ProbeTTLSSP     config.DurationKey
}{
	// PreloadInterval is now the RECONCILE interval — the full membership scan is
	// a self-heal backstop, since freshness comes from the delta path (writers
	// name the changed users/segment via AudienceInvalidateEvent; the preloader
	// re-materializes just those keys in ~1s). So it can be minutes, not seconds.
	PreloadInterval: config.RawDuration("audience.preload_interval", 5*time.Minute),
	// CacheTTL must outlive the reconcile interval, or a stable (unchanged) user —
	// only re-SET once per reconcile — would expire between reconciles.
	CacheTTL: config.RawDuration("audience.cache_ttl", 15*time.Minute),
	// The append-based audience cache writer knobs. Registered on the PIPELINE
	// schema (the writer lives there) + TierLive so they're tunable from the config
	// UI without a restart — the writer re-reads them every tick.
	ChangelogPoll:      pipelineSet.Duration("audience.changelog_poll_interval", "3s", config.TierLive, "How often the single audience cache writer (pipeline) drains audience_membership_changelog and applies SADD/SREM to Redis. The freshness knob for the append-based membership cache — seconds, coalesced per poll.", config.Since("v1.16")),
	ChangelogReconcile: pipelineSet.Duration("audience.changelog_reconcile_interval", "5m", config.TierLive, "How often the writer full-scans membership to rebuild the Redis sets (self-heal for dropped appends / batch prunes / TTL expiry). Deltas provide freshness; this is the backstop.", config.Since("v1.16")),
	ChangelogLagWarn:   pipelineSet.Duration("audience.changelog_lag_warn", "30s", config.TierLive, "If the oldest un-drained change-log row is older than this, the writer logs a WARN (the single writer is falling behind → time to shard it). Also the red threshold on the audience_cache_changelog_lag_seconds Grafana panel.", config.Since("v1.16")),
	// TaxonomyRefresh paces the SSP's warm map of public segment → IAB
	// Audience Taxonomy id used to stamp user.data on bid requests.
	TaxonomyRefresh: config.RawDuration("audience.taxonomy_refresh", 30*time.Second),

	// L1 cache hit-rate probe — Step 0 of the audience L1 cache. Registered on
	// both the DSP and SSP schemas (both read the per-bid lookup); off by default
	// so production pays only one atomic check per lookup. Flip on live for a
	// measurement, then read adtech_audience_l1_probe_{hits,misses}_total per pod.
	L1ProbeEnabledDSP: dspSet.Bool("dsp.audience_l1_probe_enabled", "false", config.TierLive, l1ProbeHelp, config.Since("v1.17")),
	L1ProbeEnabledSSP: sspSet.Bool("ssp.audience_l1_probe_enabled", "false", config.TierLive, l1ProbeHelp, config.Since("v1.17")),
	L1ProbeTTLDSP:     dspSet.Duration("dsp.audience_l1_probe_ttl", "3s", config.TierLive, l1TTLHelp, config.Since("v1.17")),
	L1ProbeTTLSSP:     sspSet.Duration("ssp.audience_l1_probe_ttl", "3s", config.TierLive, l1TTLHelp, config.Since("v1.17")),
}

const (
	l1ProbeHelp = "Shadow hit-rate probe for the proposed per-auction audience L1 cache: counts would-be cache hits/misses (adtech_audience_l1_probe_{hits,misses}_total) against the probe TTL WITHOUT changing serving — every lookup still hits Redis. The go/no-go gate for building the real cache. Off by default (one atomic check on the bid path); enable only for a measurement run."
	l1TTLHelp   = "Candidate L1 TTL the hit-rate probe measures against: a repeat lookup of the same user within this window counts as a would-be cache HIT. Set to the TTL you'd actually ship (~3s, within the audience changelog drainer lag)."
)
