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
	extras := []ExtraPurger{
		&LakePurger{BaseURL: keys.PrivacyDelete.PipelineURL.Get(cfg)},
	}
	ch, err := analytics.NewClickHouse(analytics.ClickHouseConfig{
		Addrs:    []string{keys.Reporting.ClickHouseAddr.Get(cfg)},
		Database: keys.Reporting.ClickHouseDatabase.Get(cfg),
		Username: keys.Reporting.ClickHouseUser.Get(cfg),
		Password: keys.Reporting.ClickHousePassword.Get(cfg),
		Log:      log,
	})
	if err != nil {
		log.Warn("clickhouse unreachable — freq_cap_blocks purge skipped (only exists on the clickhouse backend)", "error", err)
	} else {
		extras = append(extras, &FreqCapPurger{Store: ch})
	}
	return extras
}
