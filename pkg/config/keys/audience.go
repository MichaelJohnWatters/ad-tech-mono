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
}{
	// PreloadInterval is now the RECONCILE interval — the full membership scan is
	// a self-heal backstop, since freshness comes from the delta path (writers
	// name the changed users/segment via AudienceInvalidateEvent; the preloader
	// re-materializes just those keys in ~1s). So it can be minutes, not seconds.
	PreloadInterval: config.RawDuration("audience.preload_interval", 5*time.Minute),
	// CacheTTL must outlive the reconcile interval, or a stable (unchanged) user —
	// only re-SET once per reconcile — would expire between reconciles.
	CacheTTL: config.RawDuration("audience.cache_ttl", 15*time.Minute),
	// ChangelogPoll is how often the single audience cache writer (in pipeline)
	// drains audience_membership_changelog and applies SADD/SREM to Redis. This is
	// the freshness knob for the append-based cache — seconds, coalesced per poll.
	ChangelogPoll: config.RawDuration("audience.changelog_poll_interval", 3*time.Second),
	// ChangelogReconcile is how often the writer full-scans membership to rebuild
	// the Redis sets (self-heal for dropped appends / batch prunes / TTL expiry).
	ChangelogReconcile: config.RawDuration("audience.changelog_reconcile_interval", 5*time.Minute),
	// TaxonomyRefresh paces the SSP's warm map of public segment → IAB
	// Audience Taxonomy id used to stamp user.data on bid requests.
	TaxonomyRefresh: config.RawDuration("audience.taxonomy_refresh", 30*time.Second),
}
