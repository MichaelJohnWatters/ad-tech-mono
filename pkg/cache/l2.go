// Package cache - L2 interface (Redis).
//
// L2Cache is an interface so tests can use an in-memory implementation
// instead of requiring a real Redis connection.
package cache

import (
	"context"
	"fmt"
	"time"
)

// L2Cache is the interface for the shared cache layer (Redis).
// Supports string get/set and atomic counter operations for budgets
// and frequency caps.
type L2Cache interface {
	// Get retrieves a value by key. Returns ("", false) if not found.
	Get(ctx context.Context, key string) (string, bool, error)

	// Set stores a value with optional TTL (0 = no expiry).
	Set(ctx context.Context, key string, value string, ttl time.Duration) error

	// Delete removes a key.
	Delete(ctx context.Context, key string) error

	// SetNX sets a value only if the key does not exist (atomic).
	// Returns true if set (new), false if key already exists (duplicate).
	// Used for idempotent event processing dedup.
	SetNX(ctx context.Context, key string, value string, ttl time.Duration) (bool, error)

	// Incr atomically increments a counter by 1. Returns the new value.
	// Used for frequency cap counters.
	Incr(ctx context.Context, key string) (int64, error)

	// IncrBy atomically increments a counter by n. Returns the new value.
	IncrBy(ctx context.Context, key string, n int64) (int64, error)

	// DecrBy atomically decrements a counter by n. Returns the new value.
	// Used for budget decrements.
	DecrBy(ctx context.Context, key string, n int64) (int64, error)

	// Expire sets a TTL on an existing key.
	Expire(ctx context.Context, key string, ttl time.Duration) error

	// Ping checks the connection.
	Ping(ctx context.Context) error

	// Close closes the connection.
	Close() error
}

// MemoryL2 is an in-memory L2Cache for testing (no Redis needed).
type MemoryL2 struct {
	data    map[string]string
	expires map[string]time.Time
}

// NewMemoryL2 creates an in-memory L2 cache for testing.
func NewMemoryL2() *MemoryL2 {
	return &MemoryL2{
		data:    make(map[string]string),
		expires: make(map[string]time.Time),
	}
}

func (m *MemoryL2) Get(_ context.Context, key string) (string, bool, error) {
	if exp, ok := m.expires[key]; ok && !exp.IsZero() && time.Now().After(exp) {
		delete(m.data, key)
		delete(m.expires, key)
		return "", false, nil
	}
	v, ok := m.data[key]
	return v, ok, nil
}

func (m *MemoryL2) Set(_ context.Context, key, value string, ttl time.Duration) error {
	m.data[key] = value
	if ttl > 0 {
		m.expires[key] = time.Now().Add(ttl)
	}
	return nil
}

func (m *MemoryL2) Delete(_ context.Context, key string) error {
	delete(m.data, key)
	delete(m.expires, key)
	return nil
}

func (m *MemoryL2) SetNX(_ context.Context, key, value string, ttl time.Duration) (bool, error) {
	if _, exists := m.data[key]; exists {
		return false, nil
	}
	m.data[key] = value
	if ttl > 0 {
		m.expires[key] = time.Now().Add(ttl)
	}
	return true, nil
}

func (m *MemoryL2) Incr(_ context.Context, key string) (int64, error) {
	return m.IncrBy(nil, key, 1)
}

func (m *MemoryL2) IncrBy(_ context.Context, key string, n int64) (int64, error) {
	var current int64
	if v, ok := m.data[key]; ok {
		fmt.Sscanf(v, "%d", &current)
	}
	current += n
	m.data[key] = fmt.Sprintf("%d", current)
	return current, nil
}

func (m *MemoryL2) DecrBy(_ context.Context, key string, n int64) (int64, error) {
	return m.IncrBy(nil, key, -n)
}

func (m *MemoryL2) Expire(_ context.Context, key string, ttl time.Duration) error {
	if _, ok := m.data[key]; ok {
		m.expires[key] = time.Now().Add(ttl)
	}
	return nil
}

func (m *MemoryL2) Ping(_ context.Context) error { return nil }
func (m *MemoryL2) Close() error                 { return nil }
