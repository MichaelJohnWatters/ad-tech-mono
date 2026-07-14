//go:build !duckdb

package main

import (
	"log/slog"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
)

// maybeWrapHotCold is a no-op in the CGO-free build: the cold store needs DuckDB
// (DuckDB read_parquet over the Parquet lake), which is only compiled with `-tags duckdb`
// (see build/Dockerfile.reporting). If cold reads are requested here we log
// once and serve hot-only rather than pretend history is reachable.
func maybeWrapHotCold(store analytics.Store, cfg *config.Config, log *slog.Logger) analytics.Store {
	if keys.Reporting.ColdStoreEnabled.Get(cfg) {
		log.Warn("reporting.cold_store_enabled is set but this binary was built without the duckdb tag — serving hot-only (deep history unavailable). Build build/Dockerfile.reporting for cold reads.")
	}
	return store
}
