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
	NATSURL                      config.StringKey
	RollupEnabled                config.BoolKey
	BillingEnabled               config.BoolKey
	WarmBillingRatesPollInterval config.DurationKey
	AnalyticsBackend             config.StringKey
	ClickHouseAddr               config.StringKey
	ClickHouseDatabase           config.StringKey
	ClickHouseUser               config.StringKey
	ClickHousePassword           config.StringKey
	ClickHouseBatchConsumer      config.BoolKey
	DuckDBPath                   config.StringKey
	DedupTTL                     config.DurationKey
	SpendSnapshotEnabled         config.BoolKey
	SpendSnapshotInterval        config.DurationKey
	PacingHoldTTL                config.DurationKey
	SharedPacingCounter          config.BoolKey
	PacingReconcileInterval      config.DurationKey
	PacingCounterTTL             config.DurationKey
	ColdStoreEnabled             config.BoolKey
	HotWindow                    config.DurationKey
	QueryTimeout                 config.DurationKey

	// URL is the reporting service's base URL, bridged from REPORTING_URL
	// by Setup in pod mode. Not in the schema (see keys.go on Raw handles);
	// same for Port — ports are env/manifest territory by design.
	URL  config.StringKey
	Port config.StringKey
}{
	NATSURL:                      reportingSet.String("reporting.nats_url", "nats://localhost:4222", config.TierStatic, "NATS JetStream URL the reporting service consumes events from.", config.Since("v1.0")),
	RollupEnabled:                reportingSet.Bool("reporting.rollup_enabled", "false", config.TierLive, "Run scheduled rollups (minute/hour/day/month aggregates) inside this pod. Off in dev; on in prod where rollup ownership is centralised here.", config.Since("v1.0")),
	BillingEnabled:               reportingSet.Bool("reporting.billing_enabled", "true", config.TierLive, "Accrue billable spend in the billing ledger as AuctionWinEvents arrive. Disable to silence billing side-effects during replays.", config.Since("v1.0")),
	WarmBillingRatesPollInterval: reportingSet.Duration("cache.warm.billing_rates.poll_interval", "300s", config.TierStatic, "How often the publisher billing-rate cache refreshes. Long interval is fine — rates rarely change and a stale rate just delays the new revshare by a few minutes.", config.Since("v1.1")),
	AnalyticsBackend:             reportingSet.String("reporting.analytics_backend", "memory", config.TierStatic, "Backing store for analytics events: 'memory' (in-process, volatile — events lost on restart), 'duckdb' (embedded columnar file; needs `-tags duckdb CGO_ENABLED=1`), or 'clickhouse' (server, pure-Go client, no CGO). Memory is the unit-test/CI default; clickhouse is the full-local + prod event store (see docs/adr/0001-analytics-engines.md); duckdb is the embedded local option.", config.Since("v1.3")),
	ClickHouseAddr:               reportingSet.String("reporting.clickhouse_addr", "127.0.0.1:9000", config.TierStatic, "Comma-separated ClickHouse native-protocol addresses (host:port). Only consulted when reporting.analytics_backend=clickhouse. Locally the Tiltfile port-forwards 9000 to the in-cluster clickhouse service.", config.Since("v1.4")),
	ClickHouseDatabase:           reportingSet.String("reporting.clickhouse_database", "adtech", config.TierStatic, "ClickHouse database name. Only consulted when reporting.analytics_backend=clickhouse.", config.Since("v1.4")),
	ClickHouseUser:               reportingSet.String("reporting.clickhouse_user", "adtech", config.TierStatic, "ClickHouse username. Only consulted when reporting.analytics_backend=clickhouse.", config.Since("v1.4")),
	ClickHousePassword:           reportingSet.String("reporting.clickhouse_password", "adtech-local-dev", config.TierSecret, "ClickHouse password. Only consulted when reporting.analytics_backend=clickhouse. Prod overlays should source this from a K8s Secret.", config.Since("v1.4")),
	ClickHouseBatchConsumer:      reportingSet.Bool("reporting.clickhouse_batch_consumer", "true", config.TierStatic, "Consume the high-volume core events (impression/click/conversion/view/auction/win/media) in bulk — one atomic ClickHouse block insert per JetStream fetch instead of one INSERT per event (avoids the 'too many parts' anti-pattern). Requires a backend with bulk-insert support (clickhouse); no-op on memory/duckdb. Per-message dedup (Redis SetNX on stream sequence) makes redelivery idempotent.", config.Since("v1.5")),
	DuckDBPath:                   reportingSet.String("reporting.duckdb_path", "/tmp/adtech-analytics.duckdb", config.TierStatic, "Filesystem path for the DuckDB analytics file. Only consulted when reporting.analytics_backend=duckdb. In prod this should point at a PersistentVolumeClaim mount so the file survives pod rescheduling.", config.Since("v1.3")),
	DedupTTL:                     reportingSet.Duration("reporting.dedup_ttl", "24h", config.TierStatic, "How long a processed message's dedup marker is retained (Redis SetNX). Bounds the redelivery window the batch consumer dedups against; should exceed the JetStream stream MaxAge. Only consulted when reporting.clickhouse_batch_consumer=true.", config.Since("v1.5")),
	SpendSnapshotEnabled:         reportingSet.Bool("reporting.spend_snapshot_enabled", "true", config.TierLive, "Periodically broadcast per-campaign committed spend (settled + open reserves) on adtech.billing.campaign_spend_snapshot so DSPs reconcile pacing to billed reality. Disable to fall back to the DSP's local win-notice decrement only.", config.Since("v1.6")),
	SpendSnapshotInterval:        reportingSet.Duration("reporting.spend_snapshot_interval", "30s", config.TierLive, "How often the committed-spend snapshot is published. Shorter = DSP pacing tracks billed spend more tightly (smaller intra-snapshot over-count window) at the cost of more NATS traffic.", config.Since("v1.6")),
	PacingHoldTTL:                reportingSet.Duration("reporting.pacing_hold_ttl", "15m", config.TierLive, "How long an open reserve (impression awaiting its click/conversion/view settle) counts toward a campaign's committed spend before it's swept and released. A reserve that never settles is an impression whose billable event never arrived — freeing it stops it pacing the campaign forever.", config.Since("v1.6")),
	SharedPacingCounter:          reportingSet.Bool("reporting.shared_pacing_counter", "true", config.TierStatic, "Store per-campaign committed spend in a shared Redis counter (additive across replicas) instead of this pod's in-memory accumulator, and periodically reconcile it from the analytics store. DEFAULT ON since 2026-07-19 (correct at any replica count; Redis is a first-class dependency) — otherwise each replica sees a partial event stream and publishes conflicting pacing snapshots. Set false only to force the legacy single-replica in-memory accumulator.", config.Since("v1.7")),
	PacingReconcileInterval:      reportingSet.Duration("reporting.pacing_reconcile_interval", "60s", config.TierLive, "How often the shared pacing counter is reconciled to the authoritative per-campaign total recomputed from the analytics store (sweeps additive drift — lost/duplicated deltas). Only consulted when reporting.shared_pacing_counter=true.", config.Since("v1.7")),
	PacingCounterTTL:             reportingSet.Duration("reporting.pacing_counter_ttl", "26h", config.TierStatic, "TTL on the shared pacing counter's Redis keys, so yesterday's per-campaign counters self-expire (mirrors the DSP budget key's daily rollover). A little over a day to tolerate clock skew. Only consulted when reporting.shared_pacing_counter=true.", config.Since("v1.7")),
	ColdStoreEnabled:             reportingSet.Bool("reporting.cold_store_enabled", "false", config.TierStatic, "Route deep-history reads to the Parquet lake (DuckDB read_parquet over the active file set) below reporting.hot_window; recent reads stay on ClickHouse. Requires the clickhouse backend and a binary built with the duckdb tag (build/Dockerfile.reporting); otherwise degrades to hot-only.", config.Since("v1.4")),
	HotWindow:                    reportingSet.Duration("reporting.hot_window", "168h", config.TierStatic, "How far back the hot store (ClickHouse) is authoritative. Reads older than this fall to the cold Parquet lake; queries spanning the boundary are split and merged additively. Only used when reporting.cold_store_enabled.", config.Since("v1.4")),
	QueryTimeout:                 reportingSet.Duration("reporting.query_timeout", "2m", config.TierLive, "Per-request deadline on /v1/reporting/query (write deadline + query context). Lets deep-history cold-store reads outlive the server-wide 30s WriteTimeout, which still bounds every other route.", config.Since("v1.5")),

	URL:  config.RawString("reporting.url", routes.DefaultReportingURL),
	Port: config.RawString("reporting.port", routes.PortReporting),
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
