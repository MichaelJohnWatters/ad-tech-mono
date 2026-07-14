package config

import (
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// Schema registry. Each service owns its own keys in cmd/<svc>/config.go
// and passes them to config.Setup at boot. Setup merges the service's slice
// + the platform-shared defaultSchema() into this in-process map and writes
// the per-pod row to service_registry.schema_entries (the on-the-wire form
// the config-manager UI reads). The map is the single source of truth for
// runtime validation; there is no separate Postgres "config_schema" table
// anymore.
//
// The platform-shared keys (server.*, nats.*, database.*, redis.*, s3.*,
// debug.*, otel.*, cache.warm.*) live in defaultSchema() — they're used by
// every service, so registering them per-service would just be repeated
// boilerplate.
var (
	schemaMu       sync.RWMutex
	schemaRegistry = map[string]SchemaEntry{}
)

func init() {
	for _, e := range defaultSchema() {
		schemaRegistry[e.Key] = e
	}
}

// Register merges a service's schema slice into the in-process registry.
// Called by Setup; tests with their own schema can call this directly.
// Service field is filled in from the serviceName arg when blank.
func Register(serviceName string, entries []SchemaEntry) {
	schemaMu.Lock()
	defer schemaMu.Unlock()
	for _, e := range entries {
		if e.Service == "" {
			e.Service = serviceName
		}
		schemaRegistry[e.Key] = e
	}
}

// Tier classifies a config key by how/when it can be changed.
//
// Only Live keys are seeded into Postgres and rendered as editable in the
// UI. Static keys read from env/YAML/code-default only (changing them
// requires a redeploy). Secret keys read only from env (mounted from K8s
// Secret).
const (
	TierLive   = "live"   // operator-tunable at runtime, lives in Postgres `config` table
	TierStatic = "static" // infrastructure; env var or YAML overlay only
	TierSecret = "secret" // credentials; env var only (sourced from K8s Secret)
)

// SchemaEntry defines a config key's type, default, and validation.
// JSON tags so it round-trips through service_registry.schema_entries.
type SchemaEntry struct {
	Key         string `json:"key"`
	Type        string `json:"type"` // string, int, float, bool, duration
	Tier        string `json:"tier"` // live | static | secret
	Default     string `json:"default"`
	Description string `json:"description"`
	Service     string `json:"service"`
	Since       string `json:"since,omitempty"`
	Deprecated  bool   `json:"deprecated,omitempty"`
	ReplacedBy  string `json:"replaced_by,omitempty"`
}

// Schema returns the in-process schema registry as a slice. Each service's
// own schema (its cmd/<svc>/config.go file) + defaultSchema() are merged in
// via Register at boot. This is the source for local validation; the
// config-manager UI reads the union from service_registry.schema_entries
// instead.
func Schema() []SchemaEntry {
	schemaMu.RLock()
	defer schemaMu.RUnlock()
	out := make([]SchemaEntry, 0, len(schemaRegistry))
	for _, e := range schemaRegistry {
		out = append(out, e)
	}
	return out
}

// ErrValidation is the sentinel returned (via errors.Is) when a value
// fails schema-type validation. The gateway PUT handler uses this to
// distinguish client-input errors from server-side persistence failures
// and return 400 instead of 500.
var ErrValidation = errors.New("schema validation failed")

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
			return fmt.Errorf("%w: key %q expects a duration (e.g. 100ms, 5s), got %q", ErrValidation, entry.Key, value)
		}
	case "int":
		for _, c := range value {
			if c < '0' || c > '9' {
				return fmt.Errorf("%w: key %q expects an integer, got %q", ErrValidation, entry.Key, value)
			}
		}
	case "float":
		if _, err := strconv.ParseFloat(value, 64); err != nil {
			return fmt.Errorf("%w: key %q expects a number, got %q", ErrValidation, entry.Key, value)
		}
	case "bool":
		if value != "true" && value != "false" {
			return fmt.Errorf("%w: key %q expects true or false, got %q", ErrValidation, entry.Key, value)
		}
	}
	return nil
}
