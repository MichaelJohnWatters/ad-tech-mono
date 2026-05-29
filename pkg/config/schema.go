package config

import (
	"fmt"
	"strconv"
	"time"
)

// SchemaEntry defines a config key's type, default, and validation.
type SchemaEntry struct {
	Key          string
	Type         string // string, int, float, bool, duration
	Default      string
	Description  string
	Service      string // which service owns this key
	Since        string // version when this key was added (e.g. "v1.0")
	Deprecated   bool   // true if this key is being phased out
	ReplacedBy   string // new key name if deprecated
}

// Schema returns the full config schema for all services.
// This is the single source of truth for what config keys exist,
// their types, defaults, and which version added them.
func Schema() []SchemaEntry {
	return []SchemaEntry{
		// Gateway
		{Key: "gateway.port", Type: "string", Default: "8080", Description: "Gateway HTTP port", Service: "gateway", Since: "v1.0"},
		{Key: "gateway.jwt_signing_key", Type: "string", Default: "", Description: "JWT signing key (empty = dev mode, no auth)", Service: "gateway", Since: "v1.0"},
		{Key: "gateway.dsp_url", Type: "string", Default: "http://localhost:8082", Description: "DSP service URL", Service: "gateway", Since: "v1.0"},
		{Key: "gateway.ssp_url", Type: "string", Default: "http://localhost:8084", Description: "SSP service URL", Service: "gateway", Since: "v1.0"},
		{Key: "gateway.adserver_url", Type: "string", Default: "http://localhost:8085", Description: "Ad Server service URL", Service: "gateway", Since: "v1.0"},
		{Key: "gateway.reporting_url", Type: "string", Default: "http://localhost:8086", Description: "Reporting service URL", Service: "gateway", Since: "v1.0"},
		{Key: "gateway.exchange_url", Type: "string", Default: "http://localhost:8081", Description: "Exchange service URL", Service: "gateway", Since: "v1.0"},
		{Key: "gateway.tracker_url", Type: "string", Default: "http://localhost:8083", Description: "Tracker service URL", Service: "gateway", Since: "v1.0"},
		{Key: "gateway.config_poll_interval", Type: "duration", Default: "30s", Description: "How often to poll config from Postgres", Service: "gateway", Since: "v1.0"},

		// Exchange
		{Key: "exchange.port", Type: "string", Default: "8081", Description: "Exchange HTTP port", Service: "exchange", Since: "v1.0"},
		{Key: "exchange.channel", Type: "string", Default: "all", Description: "Channel filter (all, display, video, etc)", Service: "exchange", Since: "v1.0"},
		{Key: "exchange.bid_timeout", Type: "duration", Default: "100ms", Description: "Max time to wait for DSP bids", Service: "exchange", Since: "v1.0"},
		{Key: "exchange.dsp_endpoints", Type: "string", Default: "http://localhost:8082,http://localhost:8089,http://localhost:8090", Description: "Comma-separated DSP URLs", Service: "exchange", Since: "v1.0"},
		{Key: "exchange.nats_url", Type: "string", Default: "nats://localhost:4222", Description: "NATS JetStream URL", Service: "exchange", Since: "v1.0"},

		// DSP
		{Key: "dsp.port", Type: "string", Default: "8082", Description: "DSP HTTP port", Service: "dsp", Since: "v1.0"},
		{Key: "dsp.profile", Type: "string", Default: "internal", Description: "Campaign profile to load (internal, competitor1, competitor2)", Service: "dsp", Since: "v1.0"},

		// Tracker
		{Key: "tracker.port", Type: "string", Default: "8083", Description: "Tracker HTTP port", Service: "tracker", Since: "v1.0"},
		{Key: "tracker.nats_url", Type: "string", Default: "nats://localhost:4222", Description: "NATS JetStream URL", Service: "tracker", Since: "v1.0"},
		{Key: "tracker.reporting_url", Type: "string", Default: "http://localhost:8086", Description: "Reporting service URL (HTTP fallback)", Service: "tracker", Since: "v1.0"},
		{Key: "tracker.signing_key", Type: "string", Default: "adtech-dev-signing-key-change-in-prod", Description: "HMAC signing key for pixel URLs", Service: "tracker", Since: "v1.0"},

		// SSP
		{Key: "ssp.port", Type: "string", Default: "8084", Description: "SSP HTTP port", Service: "ssp", Since: "v1.0"},
		{Key: "ssp.exchange_url", Type: "string", Default: "http://localhost:8081", Description: "Exchange URL for bid requests", Service: "ssp", Since: "v1.0"},

		// Ad Server
		{Key: "adserver.port", Type: "string", Default: "8085", Description: "Ad Server HTTP port", Service: "adserver", Since: "v1.0"},
		{Key: "adserver.tracker_url", Type: "string", Default: "http://localhost:8083", Description: "Tracker URL for pixel generation", Service: "adserver", Since: "v1.0"},

		// Reporting
		{Key: "reporting.port", Type: "string", Default: "8086", Description: "Reporting HTTP port", Service: "reporting", Since: "v1.0"},
		{Key: "reporting.nats_url", Type: "string", Default: "nats://localhost:4222", Description: "NATS JetStream URL", Service: "reporting", Since: "v1.0"},

		// Pipeline
		{Key: "pipeline.port", Type: "string", Default: "8087", Description: "Pipeline HTTP port", Service: "pipeline", Since: "v1.0"},

		// Server timeouts (all services)
		{Key: "server.read_timeout", Type: "duration", Default: "5s", Description: "HTTP server read timeout", Service: "platform", Since: "v1.0"},
		{Key: "server.write_timeout", Type: "duration", Default: "10s", Description: "HTTP server write timeout", Service: "platform", Since: "v1.0"},
		{Key: "server.graceful_shutdown", Type: "duration", Default: "30s", Description: "Graceful shutdown period", Service: "platform", Since: "v1.0"},

		// Exchange tuning
		{Key: "exchange.max_retries", Type: "int", Default: "30", Description: "Max port bind retries on startup", Service: "exchange", Since: "v1.0"},
		{Key: "exchange.win_loss_enabled", Type: "bool", Default: "true", Description: "Send win/loss notifications to DSPs", Service: "exchange", Since: "v1.0"},

		// Tracker tuning
		{Key: "tracker.fraud_enabled", Type: "bool", Default: "true", Description: "Enable real-time fraud checks on pixel endpoints", Service: "tracker", Since: "v1.0"},
		{Key: "tracker.signature_validation", Type: "bool", Default: "false", Description: "Enforce HMAC signature validation (false = warn only)", Service: "tracker", Since: "v1.0"},
		{Key: "tracker.rate_limit_per_ip", Type: "int", Default: "60", Description: "Max requests per IP per minute before rate limiting", Service: "tracker", Since: "v1.0"},

		// DSP tuning
		{Key: "dsp.daily_budget_default", Type: "float", Default: "1000", Description: "Default daily budget for new campaigns", Service: "dsp", Since: "v1.0"},
		{Key: "dsp.max_bid_modifier", Type: "float", Default: "200", Description: "Max bid modifier percentage (safety rail)", Service: "dsp", Since: "v1.0"},

		// Reporting tuning
		{Key: "reporting.rollup_enabled", Type: "bool", Default: "false", Description: "Enable automatic rollup execution", Service: "reporting", Since: "v1.0"},
		{Key: "reporting.billing_enabled", Type: "bool", Default: "true", Description: "Enable billing on event ingestion", Service: "reporting", Since: "v1.0"},

		// Ad Server tuning
		{Key: "adserver.bandit_enabled", Type: "bool", Default: "true", Description: "Enable Thompson Sampling creative rotation", Service: "adserver", Since: "v1.0"},
		{Key: "adserver.default_creative_ttl", Type: "duration", Default: "5m", Description: "Creative metadata cache TTL", Service: "adserver", Since: "v1.0"},

		// NATS tuning
		{Key: "nats.max_reconnects", Type: "int", Default: "30", Description: "Max NATS reconnect attempts", Service: "platform", Since: "v1.0"},
		{Key: "nats.reconnect_wait", Type: "duration", Default: "1s", Description: "Wait between NATS reconnect attempts", Service: "platform", Since: "v1.0"},
		{Key: "nats.stream_max_age", Type: "duration", Default: "24h", Description: "Max age for JetStream messages", Service: "platform", Since: "v1.0"},

		// Config manager
		{Key: "config.poll_interval", Type: "duration", Default: "30s", Description: "How often services poll for config changes", Service: "platform", Since: "v1.0"},

		// Database
		{Key: "database.url", Type: "string", Default: "postgres://adtech:adtech-local-dev@localhost:5432/adtech?sslmode=disable", Description: "PostgreSQL connection URL", Service: "platform", Since: "v1.0"},
		{Key: "database.max_open_conns", Type: "int", Default: "10", Description: "Max open database connections", Service: "platform", Since: "v1.0"},
		{Key: "database.max_idle_conns", Type: "int", Default: "5", Description: "Max idle database connections", Service: "platform", Since: "v1.0"},

		// Redis
		{Key: "redis.url", Type: "string", Default: "localhost:6379", Description: "Redis connection URL", Service: "platform", Since: "v1.0"},
		{Key: "redis.pool_size", Type: "int", Default: "10", Description: "Redis connection pool size", Service: "platform", Since: "v1.0"},

		// Minio/S3
		{Key: "s3.endpoint", Type: "string", Default: "localhost:9000", Description: "S3/Minio endpoint", Service: "platform", Since: "v1.0"},
		{Key: "s3.access_key", Type: "string", Default: "adtech", Description: "S3 access key", Service: "platform", Since: "v1.0"},
		{Key: "s3.secret_key", Type: "string", Default: "adtech-local-dev", Description: "S3 secret key", Service: "platform", Since: "v1.0"},
		{Key: "s3.bucket", Type: "string", Default: "adtech-creatives", Description: "S3 bucket for creative assets", Service: "platform", Since: "v1.0"},
		{Key: "s3.use_ssl", Type: "bool", Default: "false", Description: "Use SSL for S3 connection", Service: "platform", Since: "v1.0"},
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
