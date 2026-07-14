// Package keys is the single home for every config key on the platform.
//
// Each key is declared exactly once as a typed handle — name, type, default,
// tier, and description together — grouped per service/domain. Call sites
// read through the handle instead of repeating the string and default:
//
//	timeout := keys.Reporting.QueryTimeout.Get(cfg)
//	if keys.Reporting.RollupEnabled.Get(cfg) { ... }
//	mgr.OnChange(keys.Database.MaxOpenConns.Key(), func(...) { ... })
//
// Declaring a key in a service's KeySet is what registers it: cmd/<svc>
// passes keys.<Svc>Schema() to config.Setup, which writes the schema to the
// pod's service_registry row and seeds live-tier rows in Postgres. There is
// no separate schema slice to keep in sync, and the default can no longer
// drift between the schema and the read site.
//
// Raw* handles (config.RawString etc.) cover keys intentionally not in any
// schema — the service-URL keys Setup bridges from env vars, and dynamic
// per-pod conventions. Raw string getters (cfg.Get("...")) remain only for
// keys whose names are computed at runtime.
package keys

import "github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"

// Platform-shared groups, declared in pkg/config (defaultSchema needs them;
// this package imports config, so they can't live here) and re-exported so
// everything is reachable through one import.
var (
	Server             = config.Server
	NATS               = config.NATS
	Database           = config.Database
	Redis              = config.Redis
	S3                 = config.S3
	Debug              = config.Debug
	Otel               = config.Otel
	CacheWarm          = config.CacheWarm
	ConfigPollInterval = config.ConfigPollInterval
)
