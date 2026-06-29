package config

import (
	"strconv"
	"sync/atomic"
	"time"
)

// Live[T] is a typed, lock-free holder for a config value that updates as
// the value changes in Postgres. Use it when a TierLive scalar is consumed
// inside a constructed object (an http.Client, a parsed duration on a
// counter, a compiled regex) — that is, anywhere a plain cfg.Get* read at
// boot would freeze the value.
//
// Mechanics:
//   - Store keeps the current parsed value in an atomic.Pointer for O(1)
//     lock-free reads on the hot path.
//   - Bind subscribes via Manager.OnChange so subsequent UI edits flow into
//     Store on the next poll cycle.
//
// Usage:
//
//	freqWindow := config.NewLiveDuration(mgr, cfg, "adserver.freq_cap_window", 24*time.Hour)
//	// ... per request:
//	w := freqWindow.Value()
//
// For values where a parse failure should leave the previous value in
// place (Live behaviour) rather than fall back to default, the binder
// only swaps the pointer when the new string parses cleanly. A bad edit
// in the UI is silently ignored at the consumer; the schema's Validate
// gate catches it earlier anyway.

// LiveDuration is the most common case: TTLs, timeouts, intervals.
type LiveDuration struct {
	ptr atomic.Pointer[time.Duration]
}

// NewLiveDuration constructs a LiveDuration seeded with the current value
// from cfg and subscribes to changes via mgr. Pass def for the parse
// fallback if no value is set anywhere.
func NewLiveDuration(mgr *Manager, cfg *Config, key string, def time.Duration) *LiveDuration {
	lv := &LiveDuration{}
	v := cfg.GetDuration(key, def)
	lv.ptr.Store(&v)
	mgr.OnChange(key, func(_, _, newVal string) {
		parsed, err := time.ParseDuration(newVal)
		if err != nil {
			return
		}
		lv.ptr.Store(&parsed)
	})
	return lv
}

// Value returns the current value. Safe for concurrent calls.
func (l *LiveDuration) Value() time.Duration {
	return *l.ptr.Load()
}

// LiveInt holds an int knob (rate limits, fanout sizes, frequency caps).
type LiveInt struct {
	ptr atomic.Pointer[int]
}

func NewLiveInt(mgr *Manager, cfg *Config, key string, def int) *LiveInt {
	lv := &LiveInt{}
	v := cfg.GetInt(key, def)
	lv.ptr.Store(&v)
	mgr.OnChange(key, func(_, _, newVal string) {
		parsed, err := strconv.Atoi(newVal)
		if err != nil {
			return
		}
		lv.ptr.Store(&parsed)
	})
	return lv
}

func (l *LiveInt) Value() int { return *l.ptr.Load() }

// LiveBool holds a boolean feature flag.
type LiveBool struct {
	ptr atomic.Pointer[bool]
}

func NewLiveBool(mgr *Manager, cfg *Config, key string, def bool) *LiveBool {
	lv := &LiveBool{}
	v := cfg.GetBool(key, def)
	lv.ptr.Store(&v)
	mgr.OnChange(key, func(_, _, newVal string) {
		parsed, err := strconv.ParseBool(newVal)
		if err != nil {
			return
		}
		lv.ptr.Store(&parsed)
	})
	return lv
}

func (l *LiveBool) Value() bool { return *l.ptr.Load() }

// LiveFloat holds a float64 knob (noise pct, no-bid rate, budget defaults).
type LiveFloat struct {
	ptr atomic.Pointer[float64]
}

func NewLiveFloat(mgr *Manager, cfg *Config, key string, def float64) *LiveFloat {
	lv := &LiveFloat{}
	v := cfg.GetFloat(key, def)
	lv.ptr.Store(&v)
	mgr.OnChange(key, func(_, _, newVal string) {
		parsed, err := strconv.ParseFloat(newVal, 64)
		if err != nil {
			return
		}
		lv.ptr.Store(&parsed)
	})
	return lv
}

func (l *LiveFloat) Value() float64 { return *l.ptr.Load() }

// LiveString holds a string knob. Less common — most string keys are
// infrastructure (URLs, signing keys) and live in TierStatic / TierSecret.
type LiveString struct {
	ptr atomic.Pointer[string]
}

func NewLiveString(mgr *Manager, cfg *Config, key, def string) *LiveString {
	lv := &LiveString{}
	v := cfg.Get(key, def)
	lv.ptr.Store(&v)
	mgr.OnChange(key, func(_, _, newVal string) {
		lv.ptr.Store(&newVal)
	})
	return lv
}

func (l *LiveString) Value() string { return *l.ptr.Load() }
