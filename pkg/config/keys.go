package config

import (
	"fmt"
	"strconv"
	"time"
)

// Typed config keys.
//
// A key handle couples the key name, its parsed default, and (via the KeySet
// that built it) its SchemaEntry — so the name, type, default, tier, and
// description are declared exactly once. Call sites read through the handle:
//
//	timeout := keys.Reporting.QueryTimeout.Get(cfg)
//
// instead of repeating the name and default at every read:
//
//	timeout := cfg.GetDuration("reporting.query_timeout", 2*time.Minute)
//
// All key handles live in pkg/config/keys (grouped per service/domain);
// platform-shared handles are defined in this package (platform.go) and
// re-exported there. Raw string getters (cfg.Get/GetDuration/...) remain for
// dynamic keys that can't be declared up front (serviceName+".nats_url",
// per-pod conventions).

// StringKey is a typed handle for a string config key.
type StringKey struct {
	key string
	def string
}

// Key returns the config key name (for OnChange subscriptions, logging).
func (k StringKey) Key() string { return k.key }

// Default returns the code default the handle falls back to.
func (k StringKey) Default() string { return k.def }

// Get reads the key: live value → env var → default.
func (k StringKey) Get(c *Config) string { return c.Get(k.key, k.def) }

// BoolKey is a typed handle for a bool config key.
type BoolKey struct {
	key string
	def bool
}

func (k BoolKey) Key() string        { return k.key }
func (k BoolKey) Default() bool      { return k.def }
func (k BoolKey) Get(c *Config) bool { return c.GetBool(k.key, k.def) }

// IntKey is a typed handle for an int config key.
type IntKey struct {
	key string
	def int
}

func (k IntKey) Key() string       { return k.key }
func (k IntKey) Default() int      { return k.def }
func (k IntKey) Get(c *Config) int { return c.GetInt(k.key, k.def) }

// FloatKey is a typed handle for a float64 config key.
type FloatKey struct {
	key string
	def float64
}

func (k FloatKey) Key() string           { return k.key }
func (k FloatKey) Default() float64      { return k.def }
func (k FloatKey) Get(c *Config) float64 { return c.GetFloat(k.key, k.def) }

// DurationKey is a typed handle for a duration config key.
type DurationKey struct {
	key string
	def time.Duration
}

func (k DurationKey) Key() string                 { return k.key }
func (k DurationKey) Default() time.Duration      { return k.def }
func (k DurationKey) Get(c *Config) time.Duration { return c.GetDuration(k.key, k.def) }

// KeyOption customises the SchemaEntry a KeySet builder produces.
type KeyOption func(*SchemaEntry)

// Since records the platform version that introduced the key.
func Since(version string) KeyOption {
	return func(e *SchemaEntry) { e.Since = version }
}

// OwnedBy overrides the KeySet's service label for one key — for keys that
// live in a shared set but belong to a specific service in the UI.
func OwnedBy(service string) KeyOption {
	return func(e *SchemaEntry) { e.Service = service }
}

// DeprecatedBy marks the key deprecated and names its replacement.
func DeprecatedBy(replacement string) KeyOption {
	return func(e *SchemaEntry) {
		e.Deprecated = true
		e.ReplacedBy = replacement
	}
}

// KeySet accumulates a service's SchemaEntry rows as its typed keys are
// declared. Each cmd/<svc> passes its set's Entries() to config.Setup, so
// declaring a key here is what registers it — there is no separate schema
// slice to keep in sync.
//
// Builders take the default in its canonical string form — the exact bytes
// that land in SchemaEntry.Default and the seeded Postgres row ("300s", not
// 5*time.Minute, which would re-render as "5m0s" and churn seeded rows).
// The default is parsed once at declaration; a malformed default or a
// duplicate key panics at package init so the mistake can't ship.
type KeySet struct {
	service string
	entries []SchemaEntry
}

// NewKeySet creates a KeySet whose entries default to the given service label.
func NewKeySet(service string) *KeySet {
	return &KeySet{service: service}
}

// Entries returns the accumulated schema — the slice each service passes to
// config.Setup.
func (s *KeySet) Entries() []SchemaEntry {
	out := make([]SchemaEntry, len(s.entries))
	copy(out, s.entries)
	return out
}

func (s *KeySet) add(key, typ, def, tier, description string, opts []KeyOption) {
	for _, prev := range s.entries {
		if prev.Key == key {
			panic(fmt.Sprintf("config: key %q declared twice in %s key set", key, s.service))
		}
	}
	e := SchemaEntry{Key: key, Type: typ, Tier: tier, Default: def, Description: description, Service: s.service}
	for _, o := range opts {
		o(&e)
	}
	s.entries = append(s.entries, e)
}

// String declares a string key.
func (s *KeySet) String(key, def, tier, description string, opts ...KeyOption) StringKey {
	s.add(key, "string", def, tier, description, opts)
	return StringKey{key: key, def: def}
}

// Bool declares a bool key. def must be "true" or "false".
func (s *KeySet) Bool(key, def, tier, description string, opts ...KeyOption) BoolKey {
	v, err := strconv.ParseBool(def)
	if err != nil {
		panic(fmt.Sprintf("config: key %q has invalid bool default %q", key, def))
	}
	s.add(key, "bool", def, tier, description, opts)
	return BoolKey{key: key, def: v}
}

// Int declares an int key.
func (s *KeySet) Int(key, def, tier, description string, opts ...KeyOption) IntKey {
	v, err := strconv.Atoi(def)
	if err != nil {
		panic(fmt.Sprintf("config: key %q has invalid int default %q", key, def))
	}
	s.add(key, "int", def, tier, description, opts)
	return IntKey{key: key, def: v}
}

// Float declares a float key.
func (s *KeySet) Float(key, def, tier, description string, opts ...KeyOption) FloatKey {
	v, err := strconv.ParseFloat(def, 64)
	if err != nil {
		panic(fmt.Sprintf("config: key %q has invalid float default %q", key, def))
	}
	s.add(key, "float", def, tier, description, opts)
	return FloatKey{key: key, def: v}
}

// Duration declares a duration key. def uses time.ParseDuration syntax.
func (s *KeySet) Duration(key, def, tier, description string, opts ...KeyOption) DurationKey {
	v, err := time.ParseDuration(def)
	if err != nil {
		panic(fmt.Sprintf("config: key %q has invalid duration default %q", key, def))
	}
	s.add(key, "duration", def, tier, description, opts)
	return DurationKey{key: key, def: v}
}

// Raw* build typed handles for keys that are intentionally NOT in any schema:
// service URLs bridged from env vars in Setup, and keys whose registration
// would change seeding behaviour. Promote a raw key by moving it into a
// KeySet. Unlike the KeySet builders these take typed defaults — there is no
// SchemaEntry.Default to stay byte-identical with.

// RawString builds an unregistered string handle.
func RawString(key, def string) StringKey { return StringKey{key: key, def: def} }

// RawBool builds an unregistered bool handle.
func RawBool(key string, def bool) BoolKey { return BoolKey{key: key, def: def} }

// RawInt builds an unregistered int handle.
func RawInt(key string, def int) IntKey { return IntKey{key: key, def: def} }

// RawFloat builds an unregistered float handle.
func RawFloat(key string, def float64) FloatKey { return FloatKey{key: key, def: def} }

// RawDuration builds an unregistered duration handle.
func RawDuration(key string, def time.Duration) DurationKey { return DurationKey{key: key, def: def} }
