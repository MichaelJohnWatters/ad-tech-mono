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
}
