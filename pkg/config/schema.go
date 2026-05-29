package config

import (
	"fmt"
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
	case "bool":
		if value != "true" && value != "false" {
			return fmt.Errorf("key %q expects true or false, got %q", entry.Key, value)
		}
	}
	return nil
}
