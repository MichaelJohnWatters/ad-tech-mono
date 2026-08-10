package keys

import (
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

var reportingSet = config.NewKeySet(constants.ServiceReporting)

// ReportingSchema is the reporting service's owned schema — passed to
// config.Setup by cmd/reporting. Includes the billing keys: the reporting
// service hosts pkg/billing per the architecture decision.
func ReportingSchema() []config.SchemaEntry { return reportingSet.Entries() }

// Reporting holds the reporting service's config keys.
var Reporting = struct {
	NATSURL                       config.StringKey
	RollupEnabled                 config.BoolKey
	BillingEnabled                config.BoolKey
	WarmBillingRatesPollInterval  config.DurationKey
	AnalyticsBackend              config.StringKey
	ClickHouseAddr                config.StringKey
	ClickHouseDatabase            config.StringKey
	ClickHouseUser                config.StringKey
	ClickHousePassword            config.StringKey
	ClickHouseBatchConsumer       config.BoolKey
	DedupTTL                      config.DurationKey
	SpendSnapshotEnabled          config.BoolKey
	SpendSnapshotInterval         config.DurationKey
	PacingHoldTTL                 config.DurationKey
	SharedPacingCounter           config.BoolKey
	PacingReconcileInterval       config.DurationKey
	PacingCounterTTL              config.DurationKey
	ColdStoreEnabled              config.BoolKey
	HotWindow                     config.DurationKey
	ClickHouseTTLDays             config.IntKey
	QueryTimeout                  config.DurationKey
	DataFeeMarginPct              config.FloatKey
	MarketplaceSurchargeMarginPct config.FloatKey

	// URL is the reporting service's base URL, bridged from REPORTING_URL
	// by Setup in pod mode. Not in the schema (see keys.go on Raw handles);
	// same for Port — ports are env/manifest territory by design.
	URL  config.StringKey
	Port config.StringKey
}{
	NATSURL:                       reportingSet.String("reporting.nats_url", "nats://localhost:4222", config.TierStatic, "NATS JetStream URL the reporting service consumes events from.", config.Since("v1.0")),
	RollupEnabled:                 reportingSet.Bool("reporting.rollup_enabled", "false", config.TierLive, "Run scheduled rollups (minute/hour/day/month aggregates) inside this pod. Off in dev; on in prod where rollup ownership is centralised here.", config.Since("v1.0")),
	BillingEnabled:                reportingSet.Bool("reporting.billing_enabled", "true", config.TierLive, "Accrue billable spend in the billing ledger as AuctionWinEvents arrive. Disable to silence billing side-effects during replays.", config.Since("v1.0")),
	DataFeeMarginPct:              reportingSet.Float("reporting.data_fee_margin_pct", "30", config.TierLive, "Platform margin on data-monetization fees, percent 0-100. When an external buyer wins on a request carrying a fee-bearing segment, the segment owner is credited fee×(1−margin/100) per impression; the platform retains the rest. 30 = owner keeps 70%.", config.Since("v1.11")),
	MarketplaceSurchargeMarginPct: reportingSet.Float("reporting.marketplace_surcharge_margin_pct", "30", config.TierLive, "Platform margin on data-MARKETPLACE surcharges, percent 0-100. When an INTERNAL buyer wins an impression on a campaign targeting a segment they PURCHASED (a marketplace grant), the buyer pays the listing's CPM surcharge per impression, the seller is credited surcharge×(1−margin/100), the platform keeps the rest. Separate from data_fee_margin_pct (different business model).", config.Since("v2.3")),
	WarmBillingRatesPollInterval:  reportingSet.Duration("cache.warm.billing_rates.poll_interval", "300s", config.TierStatic, "How often the publisher billing-rate cache refreshes. Long interval is fine — rates rarely change and a stale rate just delays the new revshare by a few minutes.", config.Since("v1.1")),
	AnalyticsBackend:              reportingSet.String("reporting.analytics_backend", "memory", config.TierStatic, "Backing store for analytics events: 'memory' (in-process, volatile — events lost on restart) or 'clickhouse' (server, pure-Go client, no CGO). Memory is the unit-test/CI default; clickhouse is the full-local + prod event store. DuckDB was retired as a backend in ADR 0006 (ClickHouse is the single analytical store; cold reads are ClickHouse s3() over the Parquet export).", config.Since("v1.3")),
	ClickHouseAddr:                reportingSet.String("reporting.clickhouse_addr", "127.0.0.1:9000", config.TierStatic, "Comma-separated ClickHouse native-protocol addresses (host:port). Only consulted when reporting.analytics_backend=clickhouse. Locally the Tiltfile port-forwards 9000 to the in-cluster clickhouse service.", config.Since("v1.4")),
	ClickHouseDatabase:            reportingSet.String("reporting.clickhouse_database", "adtech", config.TierStatic, "ClickHouse database name. Only consulted when reporting.analytics_backend=clickhouse.", config.Since("v1.4")),
	ClickHouseUser:                reportingSet.String("reporting.clickhouse_user", "adtech", config.TierStatic, "ClickHouse username. Only consulted when reporting.analytics_backend=clickhouse.", config.Since("v1.4")),
	ClickHousePassword:            reportingSet.String("reporting.clickhouse_password", "adtech-local-dev", config.TierSecret, "ClickHouse password. Only consulted when reporting.analytics_backend=clickhouse. Prod overlays should source this from a K8s Secret.", config.Since("v1.4")),
	ClickHouseBatchConsumer:       reportingSet.Bool("reporting.clickhouse_batch_consumer", "true", config.TierStatic, "Consume the high-volume core events (impression/click/conversion/view/auction/win/media) in bulk — one atomic ClickHouse block insert per JetStream fetch instead of one INSERT per event (avoids the 'too many parts' anti-pattern). Requires a backend with bulk-insert support (clickhouse); no-op on memory. Per-message dedup (Redis SetNX on stream sequence) makes redelivery idempotent.", config.Since("v1.5")),
	DedupTTL:                      reportingSet.Duration("reporting.dedup_ttl", "24h", config.TierStatic, "How long a processed message's dedup marker is retained (Redis SetNX). Bounds the redelivery window the batch consumer dedups against; should exceed the JetStream stream MaxAge. Only consulted when reporting.clickhouse_batch_consumer=true.", config.Since("v1.5")),
	SpendSnapshotEnabled:          reportingSet.Bool("reporting.spend_snapshot_enabled", "true", config.TierLive, "Periodically broadcast per-campaign committed spend (settled + open reserves) on adtech.billing.campaign_spend_snapshot so DSPs reconcile pacing to billed reality. Disable to fall back to the DSP's local win-notice decrement only.", config.Since("v1.6")),
	SpendSnapshotInterval:         reportingSet.Duration("reporting.spend_snapshot_interval", "30s", config.TierLive, "How often the committed-spend snapshot is published. Shorter = DSP pacing tracks billed spend more tightly (smaller intra-snapshot over-count window) at the cost of more NATS traffic.", config.Since("v1.6")),
	PacingHoldTTL:                 reportingSet.Duration("reporting.pacing_hold_ttl", "15m", config.TierLive, "How long an open reserve (impression awaiting its click/conversion/view settle) counts toward a campaign's committed spend before it's swept and released. A reserve that never settles is an impression whose billable event never arrived — freeing it stops it pacing the campaign forever.", config.Since("v1.6")),
	SharedPacingCounter:           reportingSet.Bool("reporting.shared_pacing_counter", "true", config.TierStatic, "Store per-campaign committed spend in a shared Redis counter (additive across replicas) instead of this pod's in-memory accumulator, and periodically reconcile it from the analytics store. DEFAULT ON since 2026-07-19 (correct at any replica count; Redis is a first-class dependency) — otherwise each replica sees a partial event stream and publishes conflicting pacing snapshots. Set false only to force the legacy single-replica in-memory accumulator.", config.Since("v1.7")),
	PacingReconcileInterval:       reportingSet.Duration("reporting.pacing_reconcile_interval", "60s", config.TierLive, "How often the shared pacing counter is reconciled to the authoritative per-campaign total recomputed from the analytics store (sweeps additive drift — lost/duplicated deltas). Only consulted when reporting.shared_pacing_counter=true.", config.Since("v1.7")),
	PacingCounterTTL:              reportingSet.Duration("reporting.pacing_counter_ttl", "26h", config.TierStatic, "TTL on the shared pacing counter's Redis keys, so yesterday's per-campaign counters self-expire (mirrors the DSP budget key's daily rollover). A little over a day to tolerate clock skew. Only consulted when reporting.shared_pacing_counter=true.", config.Since("v1.7")),
	ColdStoreEnabled:              reportingSet.Bool("reporting.cold_store_enabled", "false", config.TierStatic, "Route deep-history reads to the Parquet lake (DuckDB read_parquet over the active file set) below reporting.hot_window; recent reads stay on ClickHouse. Requires the clickhouse backend and a binary built with the duckdb tag (build/Dockerfile.reporting); otherwise degrades to hot-only.", config.Since("v1.4")),
	HotWindow:                     reportingSet.Duration("reporting.hot_window", "168h", config.TierStatic, "How far back the hot store (ClickHouse) is authoritative. Reads older than this fall to the cold Parquet lake; queries spanning the boundary are split and merged additively. Only used when reporting.cold_store_enabled.", config.Since("v1.4")),
	ClickHouseTTLDays:             reportingSet.Int("reporting.clickhouse_ttl_days", "30", config.TierStatic, "Raw-event ClickHouse tables (impressions/clicks/…) drop rows older than this many days — the HOT tier stays bounded; the Delta lake is the keep-forever record and cold reads serve older history. Must exceed reporting.hot_window. Rollup tables are exempt (small, long-lived aggregates). 0 = no TTL (unbounded — pre-2026-07 behaviour).", config.Since("v1.10")),
	QueryTimeout:                  reportingSet.Duration("reporting.query_timeout", "2m", config.TierLive, "Per-request deadline on /v1/reporting/query (write deadline + query context). Lets deep-history cold-store reads outlive the server-wide 30s WriteTimeout, which still bounds every other route.", config.Since("v1.5")),

	URL:  config.RawString("reporting.url", routes.DefaultReportingURL),
	Port: config.RawString("reporting.port", routes.PortReporting),
}

// Attribution holds the conversion-attribution keys. Attribution runs inside
// the reporting service (it settles conversions), so the keys live in the
// reporting schema but group under the attribution.* prefix.
var Attribution = struct {
	Enabled               config.BoolKey
	Model                 config.StringKey
	ClickThroughWindowHrs config.IntKey
	ViewThroughWindowHrs  config.IntKey
	RequireViewability    config.BoolKey
	MinViewabilitySeconds config.FloatKey
	MinIdentityConfidence config.FloatKey
	MaxResolvedIDs        config.IntKey
}{
	Enabled:               reportingSet.Bool("attribution.enabled", "true", config.TierLive, "Master switch for conversion attribution. When off, a conversion still records but is not matched to an exposure and does not settle CPA (the deterministic ctid path included).", config.Since("v2.0")),
	Model:                 reportingSet.String("attribution.model", "last_touch", config.TierLive, "Attribution model. Only 'last_touch' is implemented (most-recent qualifying touchpoint wins: last click within the click window, else last viewable impression within the view window). first_touch/linear/time_decay are future.", config.Since("v2.0")),
	ClickThroughWindowHrs: reportingSet.Int("attribution.click_through_window_hours", "720", config.TierLive, "Hours after a click within which a conversion can be credited to it (default 720 = 30 days). NOTE: real-time CPA BILLING is separately bounded by the reservation lifetime (reporting.pacing_hold_ttl / TigerBeetle void); windows beyond that credit reporting only, not a live settle.", config.Since("v2.0")),
	ViewThroughWindowHrs:  reportingSet.Int("attribution.view_through_window_hours", "168", config.TierLive, "Hours after a VIEWABLE impression within which a click-less conversion can be credited to it as view-through (default 168 = 7 days). Same billing-vs-reporting caveat as the click window.", config.Since("v2.0")),
	RequireViewability:    reportingSet.Bool("attribution.require_viewability", "true", config.TierLive, "Only count IAB-viewable impressions for view-through attribution. When false, any served impression in the window qualifies.", config.Since("v2.0")),
	MinViewabilitySeconds: reportingSet.Float("attribution.min_viewability_seconds", "1", config.TierLive, "Minimum dwell (seconds) an impression must have been viewable to qualify for view-through. The tracker already applies the IAB dwell (1s display / 2s video) when it stamps iab_viewable; this is an additional floor.", config.Since("v2.0")),
	MinIdentityConfidence: reportingSet.Float("attribution.min_identity_confidence", "1", config.TierLive, "Minimum identity-graph edge confidence to follow when resolving a conversion's visitor to platform users for attribution. Deterministic links (hashed_email/uid2/CRM/advertiser id) are 1.0; probabilistic IP+UA links are 0.5. Default 1.0 = deterministic only, so a weak/poisoned probabilistic link can't drive CPA billing.", config.Since("v2.0")),
	MaxResolvedIDs:        reportingSet.Int("attribution.max_resolved_ids", "50", config.TierLive, "Hard cap on the number of linked ids a conversion's visitor resolves to (across the 2-hop identity expansion). Bounds the view-through lookback's IN-list so a hugely-connected — possibly poisoned — identity cluster can't explode the query or over-match exposures. Truncation is logged.", config.Since("v2.0")),
}

// Billing holds the billing keys. They belong to the reporting service's
// schema (reporting hosts pkg/billing) but group under the billing.* prefix.
var Billing = struct {
	LedgerBackend                config.StringKey
	TigerBeetleAddresses         config.StringKey
	BalanceInvalidateMinInterval config.DurationKey
}{
	LedgerBackend:                reportingSet.String("billing.ledger_backend", "memory", config.TierStatic, "Backing store for the billing ledger: 'memory' (in-process slice, volatile) or 'tigerbeetle' (durable TB cluster). Memory is fine for dev/CI; tigerbeetle is the prod story.", config.Since("v1.2")),
	TigerBeetleAddresses:         reportingSet.String("billing.tigerbeetle_addresses", "127.0.0.1:3033", config.TierStatic, "Comma-separated TigerBeetle replica addresses (host:port). Only consulted when billing.ledger_backend=tigerbeetle. Locally the Tiltfile port-forwards 3033 → in-cluster 3000.", config.Since("v1.2")),
	BalanceInvalidateMinInterval: reportingSet.Duration("billing.balance_invalidate_min_interval", "5s", config.TierLive, "Per-account throttle on the advertiser-balance cache invalidates published by the billing drawdown sink.", config.Since("v1.2")),
}
