// Package store is the read-side interface for audience segment lookups
// on the bid hot path. Both the Postgres-direct implementation
// (audience/store/postgres) and the Redis-cached wrapper
// (audience/store/cached) satisfy this interface, so the DSP / SSP can be
// configured to use either depending on whether Redis is reachable.
//
// Why the interface lives in its own subpackage and not at the
// pkg/audience root: pkg/audience holds the *in-memory* fake used by
// unit tests, which has a wider API (creation, membership writes). The
// bid hot path only needs the two read methods here, and putting the
// interface alongside the storage backends keeps that minimal contract
// next to the implementations that fulfil it.
package store

import "context"

// Lookup is the per-bid segment lookup contract. Both methods must be
// safe under high concurrency and must NOT block the bid path on slow
// downstream calls — callers pass a tight context deadline (~25ms on the
// bid path); the implementation should respect that and return whatever
// it has if the deadline fires.
type Lookup interface {
	// SegmentsForUser returns public segment IDs the user is in. Used by
	// the SSP to stamp user.ext.segments on outbound bid requests so every
	// DSP in the fan-out sees the same shared audience signals.
	SegmentsForUser(ctx context.Context, userID string) ([]string, error)

	// DSPSegmentsForUser returns dsp_private segment IDs the user is in.
	// Used by the DSP's bid handler to enrich its targeting evaluation
	// with privately-held data (CRM uploads, retargeting pixels) that
	// other DSPs in the same auction never get to see.
	DSPSegmentsForUser(ctx context.Context, userID string) ([]string, error)
}
