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

// DefaultRateLimitAllowlist is the standard bypass list for the per-IP rate
// limiters (gateway/adserver/ssp/tracker): loopback + all RFC1918 private +
// link-local + IPv6 loopback/ULA. It means internal, cluster, and local-dev
// traffic is NEVER throttled — locally, where every caller SNATs to a pod/host
// private IP, the limiter is effectively inert, so `make demo`, the simulator,
// and e2e keep working with limits turned on. In prod (edge preserving the real
// client IP), only genuine public clients fall outside these ranges and get
// limited. Operators add their CDN/LB egress ranges here when applicable.
const DefaultRateLimitAllowlist = "127.0.0.0/8,::1/128,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,169.254.0.0/16,fc00::/7"

// Platform-shared groups, declared in pkg/config (defaultSchema needs them;
// this package imports config, so they can't live here) and re-exported so
// everything is reachable through one import.
var (
	Server             = config.Server
	NATS               = config.NATS
	Database           = config.Database
	Redis              = config.Redis
	S3                 = config.S3
	Platform           = config.Platform
	Debug              = config.Debug
	Otel               = config.Otel
	CacheWarm          = config.CacheWarm
	ConfigPollInterval = config.ConfigPollInterval
)
