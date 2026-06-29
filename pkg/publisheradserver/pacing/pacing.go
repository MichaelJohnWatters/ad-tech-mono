// Package pacing decides whether a publisher line item with a delivery
// commitment should serve the current impression. Unlike pkg/dsp/budget
// (which caps spend at a ceiling), this aims to hit a target by end-of-flight
// — under-delivering breaks a publisher's contract.
//
// The decision per impression is simple:
//
//	expected = committed * elapsed_fraction_of_flight
//	if actual < expected → behind pace, serve
//	if actual >= expected → on/ahead pace, defer
//
// ASAP-paced line items skip the calc and always serve while in flight.
//
// The actuals counter lives in Redis (shared across all pods of the
// publisher-adserver service). Fail-open: if Redis is unreachable we
// serve (matches the project preference for not blocking on infra
// outages — better to over-deliver than fail the request).
package pacing

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/publisheradserver"
)

// Tracker reads/writes per-line-item delivery counters and decides whether
// to serve. Holds the Redis L2 connection; safe for concurrent use.
type Tracker struct {
	l2  cache.L2Cache
	log *slog.Logger
}

// New constructs a Tracker. l2 may be a real Redis client or MemoryL2; the
// tracker treats both the same.
func New(l2 cache.L2Cache, log *slog.Logger) *Tracker {
	return &Tracker{l2: l2, log: log}
}

// ShouldServe is the per-impression pacing decision. Returns true if the
// line item is behind its expected delivery curve at `now`.
//
// House and preferred line items typically have ImpressionsCommitted == 0
// (no delivery target). Those always return true — the arbitration ladder
// decides whether they're chosen, not pacing.
func (t *Tracker) ShouldServe(li publisheradserver.PublisherLineItem, now time.Time) bool {
	if li.ImpressionsCommitted <= 0 {
		return true
	}
	if li.PacingMode == publisheradserver.PacingASAP {
		return true
	}
	expected := expectedByNow(li, now)
	if expected <= 0 {
		return true
	}
	actual, err := t.actuals(li.ID)
	if err != nil {
		t.log.Warn("pacing actuals read failed (fail-open)", "line_item", li.ID, "error", err)
		return true
	}
	return actual < expected
}

// RecordImpression atomically bumps the actuals counter for a line item.
// Called by the publisher-adserver after the direct creative was served.
// TTL on the key matches the flight end so completed line items eventually
// fall out of Redis without manual cleanup.
func (t *Tracker) RecordImpression(ctx context.Context, li publisheradserver.PublisherLineItem) error {
	key := actualsKey(li.ID)
	if _, err := t.l2.Incr(ctx, key); err != nil {
		return fmt.Errorf("incr pacing counter: %w", err)
	}
	if li.DeliveryEnd != nil {
		ttl := time.Until(*li.DeliveryEnd) + 24*time.Hour
		if ttl > 0 {
			_ = t.l2.Expire(ctx, key, ttl)
		}
	}
	return nil
}

// expectedByNow returns committed * elapsed_fraction.
// Returns 0 if the flight hasn't started; returns committed if it has ended.
func expectedByNow(li publisheradserver.PublisherLineItem, now time.Time) int64 {
	if li.DeliveryStart == nil || li.DeliveryEnd == nil {
		return 0
	}
	if now.Before(*li.DeliveryStart) {
		return 0
	}
	if now.After(*li.DeliveryEnd) {
		return li.ImpressionsCommitted
	}
	total := li.DeliveryEnd.Sub(*li.DeliveryStart)
	if total <= 0 {
		return li.ImpressionsCommitted
	}
	elapsed := now.Sub(*li.DeliveryStart)
	frac := float64(elapsed) / float64(total)
	return int64(float64(li.ImpressionsCommitted) * frac)
}

func (t *Tracker) actuals(lineItemID string) (int64, error) {
	v, ok, err := t.l2.Get(context.Background(), actualsKey(lineItemID))
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, nil
	}
	return strconv.ParseInt(v, 10, 64)
}

func actualsKey(lineItemID string) string {
	return "publisheradserver:pacing:actuals:" + lineItemID
}
