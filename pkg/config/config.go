// Package config loads configuration from three layers:
//
//  1. Code defaults (defaults.go)
//  2. Environment variables (override defaults)
//  3. Live config from Postgres (override env vars, dashboard-editable)
//
// Services call config.Load() at startup. Live config is refreshed
// via NATS cache invalidation or polling (30s fallback).
//
// Usage:
//
//	cfg := config.Load()
//	timeout := cfg.GetDuration("exchange.bid_timeout", 100*time.Millisecond)
//	maxFanout := cfg.GetInt("exchange.max_dsp_fanout", 10)
package config

import (
	"os"
	"strconv"
	"time"
)

// Config holds configuration values loaded from all layers.
// Lookup order: pod-specific -> service-level -> env var -> code default.
type Config struct {
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
	c.podID = id
}

// PodID returns the current pod identifier.
func (c *Config) PodID() string {
	return c.podID
}

// Get returns a config value.
// Checks: pod-specific -> service-level -> env var -> code default.
func (c *Config) Get(key string, defaultValue string) string {
	// Pod-specific override: "exchange.pod-abc123.bid_timeout"
	if c.podID != "" {
		podKey := podConfigKey(key, c.podID)
		if v, ok := c.values[podKey]; ok {
			return v
		}
	}
	// Service-level config
	if v, ok := c.values[key]; ok {
		return v
	}
	// Environment variable
	envKey := envKeyFromConfigKey(key)
	if v := os.Getenv(envKey); v != "" {
		return v
	}
	return defaultValue
}

// podConfigKey builds "exchange.bid_timeout" -> "exchange.pod-abc123.bid_timeout"
func podConfigKey(key, podID string) string {
	for i, c := range key {
		if c == '.' {
			return key[:i] + ".pod-" + podID + key[i:]
		}
	}
	return "pod-" + podID + "." + key
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
	c.values[key] = value
}

// SetLiveBatch sets multiple live config values at once.
func (c *Config) SetLiveBatch(values map[string]string) {
	for k, v := range values {
		c.values[k] = v
	}
}

// ClearLive removes a live config value (key reverts to env/default).
func (c *Config) ClearLive(key string) {
	delete(c.values, key)
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
