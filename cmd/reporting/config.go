package main

import (
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
)

// reportingSchema is the reporting service's owned config keys. Passed to
// config.Setup at boot; the pod writes the full schema (these + platform
// defaults) into its service_registry row.
var reportingSchema = []config.SchemaEntry{
	{Key: "reporting.nats_url", Type: "string", Tier: config.TierStatic, Default: "nats://localhost:4222", Description: "NATS JetStream URL the reporting service consumes events from.", Service: constants.ServiceReporting, Since: "v1.0"},
	{Key: "reporting.rollup_enabled", Type: "bool", Tier: config.TierLive, Default: "false", Description: "Run scheduled rollups (minute/hour/day/month aggregates) inside this pod. Off in dev; on in prod where rollup ownership is centralised here.", Service: constants.ServiceReporting, Since: "v1.0"},
	{Key: "reporting.billing_enabled", Type: "bool", Tier: config.TierLive, Default: "true", Description: "Accrue billable spend in the billing ledger as AuctionWinEvents arrive. Disable to silence billing side-effects during replays.", Service: constants.ServiceReporting, Since: "v1.0"},
	{Key: "cache.warm.billing_rates.poll_interval", Type: "duration", Tier: config.TierStatic, Default: "300s", Description: "How often the publisher billing-rate cache refreshes. Long interval is fine — rates rarely change and a stale rate just delays the new revshare by a few minutes.", Service: constants.ServiceReporting, Since: "v1.1"},
	{Key: "billing.ledger_backend", Type: "string", Tier: config.TierStatic, Default: "memory", Description: "Backing store for the billing ledger: 'memory' (in-process slice, volatile) or 'tigerbeetle' (durable TB cluster). Memory is fine for dev/CI; tigerbeetle is the prod story.", Service: constants.ServiceReporting, Since: "v1.2"},
	{Key: "reporting.analytics_backend", Type: "string", Tier: config.TierStatic, Default: "memory", Description: "Backing store for analytics events: 'memory' (in-process, volatile — events lost on restart) or 'duckdb' (embedded columnar file, durable). DuckDB requires the binary be built with `-tags duckdb CGO_ENABLED=1`. Memory is the dev/CI default; duckdb is the local/staging durable story (ClickHouse is the prod target, not yet implemented).", Service: constants.ServiceReporting, Since: "v1.3"},
	{Key: "reporting.duckdb_path", Type: "string", Tier: config.TierStatic, Default: "/tmp/adtech-analytics.duckdb", Description: "Filesystem path for the DuckDB analytics file. Only consulted when reporting.analytics_backend=duckdb. In prod this should point at a PersistentVolumeClaim mount so the file survives pod rescheduling.", Service: constants.ServiceReporting, Since: "v1.3"},
	{Key: "billing.tigerbeetle_addresses", Type: "string", Tier: config.TierStatic, Default: "127.0.0.1:3033", Description: "Comma-separated TigerBeetle replica addresses (host:port). Only consulted when billing.ledger_backend=tigerbeetle. Locally the Tiltfile port-forwards 3033 → in-cluster 3000.", Service: constants.ServiceReporting, Since: "v1.2"},
}
