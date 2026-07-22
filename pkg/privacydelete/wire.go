package privacydelete

import (
	"log/slog"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
)

// BuildExtras wires the non-Postgres purgers for the privacy binaries
// (cmd/privacy-delete, cmd/privacy-verify) from config:
//
//   - The lake purger always attaches — profile_signals/behaviour_signals
//     carry user keys in every deployment, and if the pipeline is down the
//     purge fails and retries (fail-safe, never fail-open).
//   - The freq-cap purger attaches only when ClickHouse connects: the table
//     only exists on the clickhouse analytics backend (memory/duckdb
//     deployments have nothing durable to purge), so unreachable → WARN + skip.
func BuildExtras(cfg *config.Config, log *slog.Logger) []ExtraPurger {
	// The Delta-lake LakePurger is retired with the dual-write (ADR 0006 phase
	// 5): the lake is now a derived ClickHouse→Parquet export, so the user-keyed
	// tables are purged in ClickHouse (SignalsPurger) and the affected export
	// partitions are re-derived there — no pipeline /v1/datalake/purge round-trip.
	var extras []ExtraPurger
	ch, err := analytics.NewClickHouse(analytics.ClickHouseConfig{
		Addrs:    []string{keys.Reporting.ClickHouseAddr.Get(cfg)},
		Database: keys.Reporting.ClickHouseDatabase.Get(cfg),
		Username: keys.Reporting.ClickHouseUser.Get(cfg),
		Password: keys.Reporting.ClickHousePassword.Get(cfg),
		Log:      log,
	})
	if err != nil {
		log.Warn("clickhouse unreachable — freq_cap_blocks + profile-store signal purge skipped (only exist on the clickhouse backend)", "error", err)
	} else {
		extras = append(extras, &FreqCapPurger{Store: ch})
		// ADR 0006: behaviour_signals/profile_signals live in ClickHouse (phase
		// 1) and the Parquet export (phase 4); delete both. Endpoint empty →
		// SignalsPurger skips the re-export (ClickHouse delete still runs).
		extras = append(extras, &SignalsPurger{
			Store: ch,
			ExportCfg: analytics.ExportConfig{
				Endpoint:  keys.S3.Endpoint.Get(cfg),
				Bucket:    keys.Pipeline.DatalakeBucket.Get(cfg),
				AccessKey: keys.S3.AccessKey.Get(cfg),
				SecretKey: keys.S3.SecretKey.Get(cfg),
				UseSSL:    keys.S3.UseSSL.Get(cfg),
			},
			Log: log,
		})
	}
	return extras
}
