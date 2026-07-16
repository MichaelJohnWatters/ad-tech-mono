package config

import "github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"

// Platform-shared typed keys: the config every service uses (server, nats,
// database, redis, s3, debug, otel, warm-cache fallback). Declared here —
// not in pkg/config/keys — because defaultSchema() below must see them and
// keys imports this package. pkg/config/keys re-exports the groups so call
// sites can reach everything through one import (keys.Server.ReadTimeout,
// keys.S3.Endpoint, ...).
//
// Service-owned keys that historically squatted in defaultSchema
// (report_runner.*, reporting cold-store keys, dsp balance-gate keys,
// cache.warm.freq_caps) have moved to their owning service's set in
// pkg/config/keys — registration follows ownership now.
var platformSet = NewKeySet("platform")

// NOTE: *.port keys are NOT in the schema. Ports are infrastructure (K8s
// manifest / env var territory), not runtime-tunable config. Including
// them caused per-pod port overrides (DSP_PORT=8089) to lose to the
// seeded Postgres value. Defaults live in pkg/routes; env vars like
// GATEWAY_PORT / DSP_PORT override at boot.

// Server — boot-time HTTP server config.
var Server = struct {
	ReadTimeout      DurationKey
	WriteTimeout     DurationKey
	GracefulShutdown DurationKey
}{
	ReadTimeout:      platformSet.Duration("server.read_timeout", "5s", TierStatic, "How long the HTTP server will wait for a client to send a request before timing it out.", Since("v1.0")),
	WriteTimeout:     platformSet.Duration("server.write_timeout", "10s", TierStatic, "How long the HTTP server will spend writing a response before timing out the client connection.", Since("v1.0")),
	GracefulShutdown: platformSet.Duration("server.graceful_shutdown", "30s", TierStatic, "How long shutdown waits for in-flight requests to drain before forcing the process to exit.", Since("v1.0")),
}

// NATS — boot-time NATS client reconnect / stream behaviour.
var NATS = struct {
	MaxReconnects IntKey
	ReconnectWait DurationKey
	StreamMaxAge  DurationKey

	// URL is the env-bridged NATS address (NATS_URL → nats.url in Setup).
	// Raw: registering it would change the env-override seeding path.
	URL StringKey
}{
	MaxReconnects: platformSet.Int("nats.max_reconnects", "-1", TierStatic, "Historic knob — pkg/events/natsbus now reconnects FOREVER (a capped budget permanently closed the connection after a ~30s outage and silently killed publishers until a pod bounce). Kept for schema stability; not consulted.", Since("v1.0")),
	ReconnectWait: platformSet.Duration("nats.reconnect_wait", "1s", TierStatic, "Backoff between NATS reconnect attempts.", Since("v1.0")),
	StreamMaxAge:  platformSet.Duration("nats.stream_max_age", "24h", TierStatic, "How long JetStream keeps messages on a stream before discarding them.", Since("v1.0")),

	URL: RawString("nats.url", routes.DefaultNATSURL),
}

// ConfigPollInterval — the config manager's own poll cadence (the poller
// can't reconfigure its own interval safely, hence static).
var ConfigPollInterval = platformSet.Duration("config.poll_interval", "30s", TierStatic, "How often each pod polls Postgres for live-config changes. Lower = faster picks up of UI edits, higher = less DB traffic.", Since("v1.0"))

// Database — connection settings are boot-time; pool sizes are live-applied
// via applyDBPoolKnobsLive.
var Database = struct {
	URL          StringKey
	MaxOpenConns IntKey
	MaxIdleConns IntKey
}{
	URL:          platformSet.String("database.url", routes.DefaultPostgresURL, TierStatic, "PostgreSQL connection URL used by every service. Set via DATABASE_URL env var in non-dev environments.", Since("v1.0")),
	MaxOpenConns: platformSet.Int("database.max_open_conns", "10", TierLive, "Maximum simultaneous Postgres connections this pod will open. Live: applied via db.SetMaxOpenConns on every change. Raise if you see connection-pool waits in traces.", Since("v1.0")),
	MaxIdleConns: platformSet.Int("database.max_idle_conns", "5", TierLive, "Maximum idle Postgres connections kept open between requests. Live: applied via db.SetMaxIdleConns on every change. Lower trims footprint, higher cuts reconnect cost.", Since("v1.0")),
}

// Redis — connection is infra; password is secret.
var Redis = struct {
	URL      StringKey
	Password StringKey
	DB       IntKey
	PoolSize IntKey
}{
	URL:      platformSet.String("redis.url", routes.DefaultRedisAddr, TierStatic, "Redis address (host:port). Used for L2 cache, budgets, frequency caps, and dedup keys.", Since("v1.0")),
	Password: platformSet.String("redis.password", "", TierSecret, "Redis password. Empty in dev (no auth); sourced from K8s Secret in staging/prod.", Since("v1.1")),
	DB:       platformSet.Int("redis.db", "0", TierStatic, "Redis logical DB number (0-15). Use a non-zero DB to isolate environments sharing one Redis.", Since("v1.1")),
	PoolSize: platformSet.Int("redis.pool_size", "10", TierStatic, "Maximum simultaneous Redis connections this pod will open. Raise for high-RPS pods seeing pool waits.", Since("v1.0")),
}

// S3 — Minio locally, real S3 in staging/prod. Endpoint + bucket = infra,
// secret_key = secret.
var S3 = struct {
	Endpoint  StringKey
	AccessKey StringKey
	SecretKey StringKey
	Bucket    StringKey
	Region    StringKey
	UseSSL    BoolKey
}{
	Endpoint:  platformSet.String("s3.endpoint", routes.DefaultMinioEndpoint, TierStatic, "S3/Minio endpoint. Locally this is Minio on 9000; in prod set to s3.amazonaws.com or the region-specific endpoint.", Since("v1.0")),
	AccessKey: platformSet.String("s3.access_key", "adtech", TierStatic, "S3/Minio access key (IAM user / Minio root user).", Since("v1.0")),
	SecretKey: platformSet.String("s3.secret_key", "adtech-local-dev", TierSecret, "S3/Minio secret key. Sourced from K8s Secret in non-dev environments.", Since("v1.0")),
	Bucket:    platformSet.String("s3.bucket", "adtech-creatives", TierStatic, "Bucket name that holds creative HTML/image assets.", Since("v1.0")),
	Region:    platformSet.String("s3.region", "us-east-1", TierStatic, "AWS region for S3. Minio ignores this; required for real S3.", Since("v1.1")),
	UseSSL:    platformSet.Bool("s3.use_ssl", "false", TierStatic, "Connect to S3/Minio over HTTPS. False for local Minio, true for real S3.", Since("v1.0")),
}

// Debug — static because registering/unregistering routes requires restart.
var Debug = struct {
	EndpointsEnabled BoolKey
}{
	EndpointsEnabled: platformSet.Bool("debug.endpoints_enabled", "true", TierStatic, "Expose /debug/* routes (warm-cache refresh, internal state dumps). Must be false in production.", Since("v1.1")),
}

// Otel — static because exporter/sampler are built at Init.
var Otel = struct {
	Endpoint       StringKey
	SampleRatio    FloatKey
	ServiceVersion StringKey
}{
	Endpoint:       platformSet.String("otel.endpoint", "localhost:4318", TierStatic, "OTLP/HTTP collector endpoint (Jaeger in dev). Leave empty to disable tracing entirely.", Since("v1.1")),
	SampleRatio:    platformSet.Float("otel.sample_ratio", "1.0", TierStatic, "Fraction of traces to record (0.0-1.0). 1.0 samples every request; lower in high-RPS services like the tracker.", Since("v1.1")),
	ServiceVersion: platformSet.String("otel.service_version", "dev", TierStatic, "Version string reported to the OTel collector. Usually set to the build SHA at deploy time.", Since("v1.1")),
}

// CacheWarm — fallback refresh interval for warm caches with no per-cache
// override. Per-cache overrides (cache.warm.campaigns.*, ...) live in the
// owning service's key set.
var CacheWarm = struct {
	PollInterval DurationKey

	// SecretsPollInterval is the secrets cache's specific-key probe (0 =
	// fall through to PollInterval). Raw: never registered in any schema.
	SecretsPollInterval DurationKey
}{
	PollInterval: platformSet.Duration("cache.warm.poll_interval", "30s", TierStatic, "Fallback refresh interval for warm caches with no per-cache override. Most caches set their own.", Since("v1.1")),

	SecretsPollInterval: RawDuration("cache.warm.secrets.poll_interval", 0),
}

// defaultSchema is the platform-shared key set appended to every service's
// schema in Setup. Derived from the typed key declarations above — declaring
// a key in platformSet is what puts it here.
func defaultSchema() []SchemaEntry {
	return platformSet.Entries()
}
