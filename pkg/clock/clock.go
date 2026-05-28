// Package clock provides a time abstraction for the entire platform.
//
// Rule: NO time.Now() calls in application code. Use clock.Now() instead.
// This enables deterministic testing of all time-dependent features:
// pacing, dayparting, attribution windows, frequency cap TTLs,
// reservation expiry, data retention, session timeouts, and auction timeouts.
package clock

import (
	"sync"
	"time"
)

// Clock abstracts time operations. All services receive a Clock at startup
// via dependency injection. Production uses Real, tests use Fake.
type Clock interface {
	// Now returns the current time.
	Now() time.Time

	// Since returns the duration since t.
	Since(t time.Time) time.Duration

	// Until returns the duration until t.
	Until(t time.Time) time.Duration

	// After waits for the duration to elapse and then sends the current
	// time on the returned channel.
	After(d time.Duration) <-chan time.Time

	// NewTicker returns a new Ticker that sends the time on its channel
	// at the specified interval.
	NewTicker(d time.Duration) *time.Ticker
}

// Real implements Clock using the actual system time.
// Use this in production.
type Real struct{}

// Now returns the current system time.
func (Real) Now() time.Time { return time.Now() }

// Since returns the elapsed time since t.
func (Real) Since(t time.Time) time.Duration { return time.Since(t) }

// Until returns the duration until t.
func (Real) Until(t time.Time) time.Duration { return time.Until(t) }

// After waits for d to elapse then sends the time.
func (Real) After(d time.Duration) <-chan time.Time { return time.After(d) }

// NewTicker returns a standard time.Ticker.
func (Real) NewTicker(d time.Duration) *time.Ticker { return time.NewTicker(d) }

// Fake implements Clock with a controllable time for testing.
// Thread-safe - can be advanced from a different goroutine than the one reading.
type Fake struct {
	mu      sync.RWMutex
	current time.Time
}

// NewFake creates a Fake clock set to the given time.
func NewFake(t time.Time) *Fake {
	return &Fake{current: t}
}

// Now returns the fake current time.
func (f *Fake) Now() time.Time {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.current
}

// Since returns the duration since t based on the fake current time.
func (f *Fake) Since(t time.Time) time.Duration {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.current.Sub(t)
}

// Until returns the duration until t based on the fake current time.
func (f *Fake) Until(t time.Time) time.Duration {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return t.Sub(f.current)
}

// After returns a channel that receives immediately (fake time doesn't wait).
func (f *Fake) After(_ time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	f.mu.RLock()
	ch <- f.current
	f.mu.RUnlock()
	return ch
}

// NewTicker returns a real ticker (fake clock doesn't control ticker intervals).
// For tests that need controllable tickers, use Advance() to simulate time passing.
func (f *Fake) NewTicker(d time.Duration) *time.Ticker {
	return time.NewTicker(d)
}

// Advance moves the fake clock forward by the given duration.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.current = f.current.Add(d)
}

// Set sets the fake clock to a specific time.
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.current = t
}
