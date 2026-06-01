package config

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// Schema registry — replaces the previous "edit pkg/config/schema.go to add
// a key" model. Each service owns its own keys and calls PublishSchema at
// boot. PublishSchema both writes to the config_schema table (so the
// gateway's config-manager UI can render every service's keys via a single
// SELECT) and merges into this in-process registry (so each service can
// Validate / get defaults locally without a DB roundtrip).
//
// The platform-shared keys (server.*, nats.*, database.*, redis.*, s3.*,
// debug.*, otel.*, cache.warm.*) stay in defaultSchema() below — they're
// used by every service, so registering them per-service would just be
// repeated boilerplate. Service-specific keys (dsp.*, exchange.*, etc.)
// live in their respective cmd/<svc>/config.go file.
var (
	schemaMu       sync.RWMutex
	schemaRegistry = map[string]SchemaEntry{}
)

func init() {
	// Seed the registry with the platform defaults so single-service tests
	// (which don't go through the boot Setup path) can still Validate.
	for _, e := range defaultSchema() {
		schemaRegistry[e.Key] = e
	}
}

// PublishSchema registers a service's schema entries: writes them to the
// config_schema table (so the config-manager UI sees them) and merges into
// the in-process registry (so Validate / defaults work locally). Atomic per
// service — old rows for the service are deleted before fresh inserts so
// removing a key actually removes it from the published surface.
//
// nil db = in-process registry only. Useful for tests + dev environments
// without Postgres reachable at boot.
func PublishSchema(ctx context.Context, db *sql.DB, service string, entries []SchemaEntry) error {
	schemaMu.Lock()
	for _, e := range entries {
		if e.Service == "" {
			e.Service = service
		}
		schemaRegistry[e.Key] = e
	}
	schemaMu.Unlock()

	if db == nil {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "DELETE FROM config_schema WHERE service = $1", service); err != nil {
		return fmt.Errorf("delete previous %s rows: %w", service, err)
	}
	const ins = `
INSERT INTO config_schema (key, type, tier, "default", description, service, since_version, deprecated, replaced_by, published_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now(), now())
ON CONFLICT (key) DO UPDATE SET
    type = EXCLUDED.type, tier = EXCLUDED.tier, "default" = EXCLUDED."default",
    description = EXCLUDED.description, service = EXCLUDED.service,
    since_version = EXCLUDED.since_version, deprecated = EXCLUDED.deprecated,
    replaced_by = EXCLUDED.replaced_by, updated_at = now()`
	for _, e := range entries {
		svc := e.Service
		if svc == "" {
			svc = service
		}
		if _, err := tx.ExecContext(ctx, ins,
			e.Key, e.Type, e.Tier, e.Default, e.Description,
			svc, e.Since, e.Deprecated, e.ReplacedBy,
		); err != nil {
			return fmt.Errorf("insert %s: %w", e.Key, err)
		}
	}
	return tx.Commit()
}

// PublishSchemaWithURL is the convenience wrapper services call from boot:
// opens a short-lived DB connection to dbURL, publishes, closes. nil-tolerant
// on any failure — the in-process registry update still happens so Validate /
// GetDefault calls within the same service continue working. dbURL == "" or
// any connection failure logs a warn and proceeds without DB publish.
func PublishSchemaWithURL(dbURL, service string, entries []SchemaEntry, log Logger) {
	if dbURL == "" {
		_ = PublishSchema(context.Background(), nil, service, entries)
		return
	}
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Warn("publish schema: db open failed (in-process only)", "service", service, "error", err)
		_ = PublishSchema(context.Background(), nil, service, entries)
		return
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := PublishSchema(ctx, db, service, entries); err != nil {
		log.Warn("publish schema failed (continuing with in-process registry)", "service", service, "error", err)
	}
}

// Logger is the small interface PublishSchemaWithURL uses — keeps this
// package free of an slog dependency that creates an import cycle with
// pkg/logger.
type Logger interface {
	Warn(msg string, args ...any)
}

// LoadPublishedSchema reads the full union of published schemas from
// config_schema into the in-process registry. Called by the gateway at
// boot so its config-manager UI sees every service's keys without
// importing every cmd/<svc>.
func LoadPublishedSchema(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return nil
	}
	rows, err := db.QueryContext(ctx, `
SELECT key, type, tier, "default", description, service,
       since_version, deprecated, replaced_by
FROM config_schema`)
	if err != nil {
		return fmt.Errorf("query published schema: %w", err)
	}
	defer rows.Close()

	loaded := map[string]SchemaEntry{}
	for rows.Next() {
		var e SchemaEntry
		if err := rows.Scan(&e.Key, &e.Type, &e.Tier, &e.Default, &e.Description,
			&e.Service, &e.Since, &e.Deprecated, &e.ReplacedBy); err != nil {
			return fmt.Errorf("scan: %w", err)
		}
		loaded[e.Key] = e
	}
	if err := rows.Err(); err != nil {
		return err
	}

	schemaMu.Lock()
	defer schemaMu.Unlock()
	for k, e := range loaded {
		schemaRegistry[k] = e
	}
	return nil
}

// Tier classifies a config key by how/when it can be changed.
//
// The classification is enforced by the config manager: only Live keys
// are seeded into Postgres and rendered as editable in the UI. Static
// keys read from env/YAML/code-default only (changing them requires a
// redeploy). Secret keys read only from env (mounted from K8s Secret).
//
// Mixing tiers in one table is what produced the *.port bug: ports got
// seeded into Postgres on every pod register, and Postgres values won
// over the env vars Tilt set, so per-pod port overrides silently failed.
const (
	TierLive   = "live"   // operator-tunable at runtime, lives in Postgres `config` table
	TierStatic = "static" // infrastructure; env var or YAML overlay only
	TierSecret = "secret" // credentials; env var only (sourced from K8s Secret)
)

// SchemaEntry defines a config key's type, default, and validation.
type SchemaEntry struct {
	Key         string
	Type        string // string, int, float, bool, duration
	Tier        string // live | static | secret — see TierLive/TierStatic/TierSecret
	Default     string
	Description string
	Service     string // which service owns this key
	Since       string // version when this key was added (e.g. "v1.0")
	Deprecated  bool   // true if this key is being phased out
	ReplacedBy  string // new key name if deprecated
}

// Schema returns the full union of registered config entries. Sourced from
// the in-process registry (seeded by defaultSchema() at init + each service's
// PublishSchema call at boot + LoadPublishedSchema reads from Postgres). No
// longer the hardcoded "all services" slice.
func Schema() []SchemaEntry {
	schemaMu.RLock()
	defer schemaMu.RUnlock()
	out := make([]SchemaEntry, 0, len(schemaRegistry))
	for _, e := range schemaRegistry {
		out = append(out, e)
	}
	return out
}

// defaultSchema is the platform-shared key set: keys that every service uses
// (server, nats, config, database, redis, s3, debug, otel, generic
// cache.warm.*). Kept here rather than registered per-service to avoid
// repeating the same rows in every cmd/<svc>/config.go.
//
// Service-specific keys (dsp.*, exchange.*, tracker.*, ssp.*, adserver.*,
// reporting.*, gateway.*) used to live here; they now own their own
// cmd/<svc>/config.go file and publish via config.PublishSchemaWithURL at
// boot. The gateway calls LoadPublishedSchema to merge everyone's keys
// back into the in-process registry for the config-manager UI.
//
// NOTE: *.port keys are NOT in the schema. Ports are infrastructure
// (K8s manifest / env var territory), not runtime-tunable config.
// Including them caused per-pod port overrides (DSP_PORT=8089) to lose
// to the seeded Postgres value. Defaults live in pkg/routes; env vars
// like GATEWAY_PORT / DSP_PORT override at boot.
func defaultSchema() []SchemaEntry {
	return []SchemaEntry{
		// Server timeouts (boot-time HTTP server config)
		{Key: "server.read_timeout", Type: "duration", Tier: TierStatic, Default: "5s", Description: "HTTP server read timeout", Service: "platform", Since: "v1.0"},
		{Key: "server.write_timeout", Type: "duration", Tier: TierStatic, Default: "10s", Description: "HTTP server write timeout", Service: "platform", Since: "v1.0"},
		{Key: "server.graceful_shutdown", Type: "duration", Tier: TierStatic, Default: "30s", Description: "Graceful shutdown period", Service: "platform", Since: "v1.0"},

		// NATS reconnect / stream behavior (boot-time NATS client config)
		{Key: "nats.max_reconnects", Type: "int", Tier: TierStatic, Default: "30", Description: "Max NATS reconnect attempts", Service: "platform", Since: "v1.0"},
		{Key: "nats.reconnect_wait", Type: "duration", Tier: TierStatic, Default: "1s", Description: "Wait between NATS reconnect attempts", Service: "platform", Since: "v1.0"},
		{Key: "nats.stream_max_age", Type: "duration", Tier: TierStatic, Default: "24h", Description: "Max age for JetStream messages", Service: "platform", Since: "v1.0"},

		// Config manager (the poller itself can't reconfigure its own interval safely)
		{Key: "config.poll_interval", Type: "duration", Tier: TierStatic, Default: "30s", Description: "How often services poll for config changes", Service: "platform", Since: "v1.0"},

		// Database — connection settings are boot-time, credentials in URL are secret-adjacent.
		{Key: "database.url", Type: "string", Tier: TierStatic, Default: "postgres://adtech:adtech-local-dev@localhost:5432/adtech?sslmode=disable", Description: "PostgreSQL connection URL", Service: "platform", Since: "v1.0"},
		{Key: "database.max_open_conns", Type: "int", Tier: TierStatic, Default: "10", Description: "Max open database connections", Service: "platform", Since: "v1.0"},
		{Key: "database.max_idle_conns", Type: "int", Tier: TierStatic, Default: "5", Description: "Max idle database connections", Service: "platform", Since: "v1.0"},

		// Redis — connection is infra; password is secret.
		{Key: "redis.url", Type: "string", Tier: TierStatic, Default: "localhost:6379", Description: "Redis connection URL", Service: "platform", Since: "v1.0"},
		{Key: "redis.password", Type: "string", Tier: TierSecret, Default: "", Description: "Redis password (empty for no auth)", Service: "platform", Since: "v1.1"},
		{Key: "redis.db", Type: "int", Tier: TierStatic, Default: "0", Description: "Redis DB number", Service: "platform", Since: "v1.1"},
		{Key: "redis.pool_size", Type: "int", Tier: TierStatic, Default: "10", Description: "Redis connection pool size", Service: "platform", Since: "v1.0"},

		// Minio/S3 — endpoint + bucket = infra, secret_key = secret.
		{Key: "s3.endpoint", Type: "string", Tier: TierStatic, Default: "localhost:9000", Description: "S3/Minio endpoint", Service: "platform", Since: "v1.0"},
		{Key: "s3.access_key", Type: "string", Tier: TierStatic, Default: "adtech", Description: "S3 access key", Service: "platform", Since: "v1.0"},
		{Key: "s3.secret_key", Type: "string", Tier: TierSecret, Default: "adtech-local-dev", Description: "S3 secret key", Service: "platform", Since: "v1.0"},
		{Key: "s3.bucket", Type: "string", Tier: TierStatic, Default: "adtech-creatives", Description: "S3 bucket for creative assets", Service: "platform", Since: "v1.0"},
		{Key: "s3.region", Type: "string", Tier: TierStatic, Default: "us-east-1", Description: "S3 region", Service: "platform", Since: "v1.1"},
		{Key: "s3.use_ssl", Type: "bool", Tier: TierStatic, Default: "false", Description: "Use SSL for S3 connection", Service: "platform", Since: "v1.0"},

		// Debug endpoints (static — registering/unregistering routes requires restart)
		{Key: "debug.endpoints_enabled", Type: "bool", Tier: TierStatic, Default: "true", Description: "Expose /debug/* endpoints (cache refresh, etc). Set false in production.", Service: "platform", Since: "v1.1"},

		// OpenTelemetry / tracing (static — exporter/sampler are built at Init)
		{Key: "otel.endpoint", Type: "string", Tier: TierStatic, Default: "localhost:4318", Description: "OTLP/HTTP collector endpoint (Jaeger). Empty = tracing disabled.", Service: "platform", Since: "v1.1"},
		{Key: "otel.sample_ratio", Type: "float", Tier: TierStatic, Default: "1.0", Description: "Trace sample ratio (0.0-1.0). 1.0 = sample every request (dev default).", Service: "platform", Since: "v1.1"},
		{Key: "otel.service_version", Type: "string", Tier: TierStatic, Default: "dev", Description: "Service version reported to the collector", Service: "platform", Since: "v1.1"},

		// Warm cache default poll interval — generic fallback used by every
		// service. Per-cache overrides (cache.warm.campaigns.*,
		// cache.warm.creatives.*, etc.) live in the owning service's
		// cmd/<svc>/config.go and override this default at boot.
		{Key: "cache.warm.poll_interval", Type: "duration", Tier: TierStatic, Default: "30s", Description: "Default poll interval for warm caches", Service: "platform", Since: "v1.1"},
	}
}

// Validate checks if a value is valid for a given config key.
func Validate(key, value string) error {
	for _, entry := range Schema() {
		if entry.Key == key {
			return validateType(entry, value)
		}
	}
	// Unknown keys are allowed (forward compatibility)
	return nil
}

func validateType(entry SchemaEntry, value string) error {
	switch entry.Type {
	case "duration":
		if _, err := time.ParseDuration(value); err != nil {
			return fmt.Errorf("key %q expects a duration (e.g. 100ms, 5s), got %q", entry.Key, value)
		}
	case "int":
		for _, c := range value {
			if c < '0' || c > '9' {
				return fmt.Errorf("key %q expects an integer, got %q", entry.Key, value)
			}
		}
	case "float":
		if _, err := strconv.ParseFloat(value, 64); err != nil {
			return fmt.Errorf("key %q expects a number, got %q", entry.Key, value)
		}
	case "bool":
		if value != "true" && value != "false" {
			return fmt.Errorf("key %q expects true or false, got %q", entry.Key, value)
		}
	}
	return nil
}
