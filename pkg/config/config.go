// Package config loads configuration from three layers:
//
//  1. Code defaults (defaults.go)
//  2. Environment variables (override defaults)
//  3. Live config from Postgres (override env vars, dashboard-editable)
//
// Services call config.Load() at startup. Live config is refreshed
// via NATS cache invalidation or polling (30s fallback).
//
// Every key is declared once as a typed handle in pkg/config/keys (name,
// type, default, tier, description together — see keys.go in this package
// for the mechanism). Call sites read through the handle:
//
//	timeout := keys.Exchange.BidTimeout.Get(cfg)
//	maxFanout := keys.Exchange.MaxDSPFanout.Get(cfg)
//
// The raw string getters below remain for dynamic keys whose names are
// computed at runtime (serviceName+".nats_url", per-pod conventions) and
// for sentinel reads that deliberately differ from the schema default
// (cfg.Get(keys.Database.URL.Key(), "")).
package config

import (
	"os"
	"strconv"
	"sync"
	"time"
)

// Config holds configuration values loaded from all layers.
// Lookup order: pod-specific -> service-level -> env var -> code default.
//
// All accesses to `values` go through the RWMutex so the Manager's 30s
// poll (which writes new live values via SetLive) is safe against concurrent
// reads from `cfg.Get*` on hot paths. The previous version had a latent
// race here — concurrent map read+write in Go is undefined behaviour and
// `-race` would flag it.
type Config struct {
	mu     sync.RWMutex
	values map[string]string
	podID  string
}

// Load creates a new Config.
func Load() *Config {
	podID := os.Getenv("POD_NAME")
	if podID == "" {
		podID = os.Getenv("HOSTNAME")
	}
	return &Config{
		values: make(map[string]string),
		podID:  podID,
	}
}

// SetPodID sets the pod identifier for pod-level config lookups.
func (c *Config) SetPodID(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.podID = id
}

// PodID returns the current pod identifier.
func (c *Config) PodID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.podID
}

// Get returns a config value.
//
// Resolution: in-memory value (populated by the manager from a pod-scoped
// Postgres fetch) → env var → code default.
//
// The pod scoping happens upstream: PostgresSource.FetchAllForPod returns
// only this pod's rows (plus legacy globals) so c.values already contains
// the right value for this pod. The old key-prefix trick
// ("exchange.pod-X.bid_timeout") is gone — same key, scoped row.
func (c *Config) Get(key string, defaultValue string) string {
	c.mu.RLock()
	v, ok := c.values[key]
	c.mu.RUnlock()
	if ok {
		return v
	}
	envKey := envKeyFromConfigKey(key)
	if v := os.Getenv(envKey); v != "" {
		return v
	}
	return defaultValue
}

// GetInt returns an integer config value.
func (c *Config) GetInt(key string, defaultValue int) int {
	s := c.Get(key, "")
	if s == "" {
		return defaultValue
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return defaultValue
	}
	return v
}

// GetFloat returns a float64 config value.
func (c *Config) GetFloat(key string, defaultValue float64) float64 {
	s := c.Get(key, "")
	if s == "" {
		return defaultValue
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return defaultValue
	}
	return v
}

// GetBool returns a boolean config value.
func (c *Config) GetBool(key string, defaultValue bool) bool {
	s := c.Get(key, "")
	if s == "" {
		return defaultValue
	}
	v, err := strconv.ParseBool(s)
	if err != nil {
		return defaultValue
	}
	return v
}

// GetDuration returns a time.Duration config value.
func (c *Config) GetDuration(key string, defaultValue time.Duration) time.Duration {
	s := c.Get(key, "")
	if s == "" {
		return defaultValue
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return defaultValue
	}
	return v
}

// SetLive sets a live config value (from Postgres or NATS invalidation).
func (c *Config) SetLive(key, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.values[key] = value
}

// SetLiveBatch sets multiple live config values at once.
func (c *Config) SetLiveBatch(values map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, v := range values {
		c.values[k] = v
	}
}

// ClearLive removes a live config value (key reverts to env/default).
func (c *Config) ClearLive(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.values, key)
}

// rawGet returns the current live value and whether the key existed.
// Used by Manager to detect changes during poll/applyChanges. Distinct
// from Get because Get folds env/default into the result.
func (c *Config) rawGet(key string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.values[key]
	return v, ok
}

// snapshot returns a copy of the live values map. Used by Manager.All for
// the UI and by tests. Allocates — don't call on hot paths.
func (c *Config) snapshot() map[string]string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]string, len(c.values))
	for k, v := range c.values {
		out[k] = v
	}
	return out
}

// envKeyFromConfigKey converts "exchange.bid_timeout" to "EXCHANGE_BID_TIMEOUT"
func envKeyFromConfigKey(key string) string {
	result := make([]byte, len(key))
	for i, c := range key {
		if c == '.' {
			result[i] = '_'
		} else if c >= 'a' && c <= 'z' {
			result[i] = byte(c - 32) // uppercase
		} else {
			result[i] = byte(c)
		}
	}
	return string(result)
}
