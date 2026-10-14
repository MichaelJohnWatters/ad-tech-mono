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

// DSPBatchLookup is an optional Lookup capability: dsp_private segments for
// many user ids in ONE round trip. The DSP folds a user's identity-graph-
// linked ids into its private-segment lookup (up to identity_max_linked);
// serially that was one Redis round trip per id inside the bid deadline.
// Decorators (L1 cache, probe) must forward it or the chain silently
// degrades to serial lookups.
type DSPBatchLookup interface {
	// DSPSegmentsForUsers returns one segment slice per id, index-aligned.
	DSPSegmentsForUsers(ctx context.Context, userIDs []string) ([][]string, error)
}

// DSPSegmentsForUsers uses l's batch capability when present, else falls back
// to serial DSPSegmentsForUser calls. Serial-path errors degrade per id (that
// id reads as "no segments"), matching the bid path's best-effort contract.
func DSPSegmentsForUsers(ctx context.Context, l Lookup, userIDs []string) ([][]string, error) {
	if b, ok := l.(DSPBatchLookup); ok {
		return b.DSPSegmentsForUsers(ctx, userIDs)
	}
	out := make([][]string, len(userIDs))
	for i, id := range userIDs {
		segs, err := l.DSPSegmentsForUser(ctx, id)
		if err != nil {
			continue
		}
		out[i] = segs
	}
	return out, nil
}

// PublicBatchLookup is the public-visibility twin of DSPBatchLookup: the SSP
// looks up user + household + hashed-email public segments per request —
// three Redis round trips (three goroutines) collapsed into one pipelined call.
type PublicBatchLookup interface {
	// SegmentsForUsers returns one public segment slice per id, index-aligned.
	SegmentsForUsers(ctx context.Context, userIDs []string) ([][]string, error)
}

// SegmentsForUsers uses l's public batch capability when present, else serial
// SegmentsForUser calls degrading per id (a failed id reads as no segments).
func SegmentsForUsers(ctx context.Context, l Lookup, userIDs []string) ([][]string, error) {
	if b, ok := l.(PublicBatchLookup); ok {
		return b.SegmentsForUsers(ctx, userIDs)
	}
	out := make([][]string, len(userIDs))
	for i, id := range userIDs {
		segs, err := l.SegmentsForUser(ctx, id)
		if err != nil {
			continue
		}
		out[i] = segs
	}
	return out, nil
}
