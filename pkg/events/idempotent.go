// Package events - idempotent consumer wrapper.
//
// NATS JetStream guarantees at-least-once delivery. If a consumer processes
// a message but crashes before acking, NATS redelivers. Without dedup,
// this causes double-billing, double-counting, etc.
//
// IdempotentHandler wraps any Handler with Redis SetNX-based deduplication.
// If a message ID has been seen before, it's acked silently (already processed).
//
// This is MANDATORY for all consumers. The Subscribe method wraps handlers
// with this automatically.
package events

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// DedupStore checks and records processed message IDs.
// Implemented by Redis (SetNX with TTL).
type DedupStore interface {
	// MarkProcessed attempts to mark a message as processed.
	// Returns true if this is the first time (new), false if already processed (duplicate).
	MarkProcessed(ctx context.Context, key string, ttl time.Duration) (bool, error)

	// UnmarkProcessed removes the processed marker (for retry on failure).
	UnmarkProcessed(ctx context.Context, key string) error
}

// BatchDedupStore is an optional DedupStore capability: MarkProcessed for many
// keys in ONE round trip (a pipeline, executed in order — so a key repeated
// within the batch is new only at its first position, exactly like N serial
// calls). Batch consumers otherwise paid one sequential Redis SETNX per
// message (two with a business key), which capped reporting's consumer
// throughput below the offered event rate (2026-10-14: 580k dsp_call +
// 144k impression pending at 150rps).
type BatchDedupStore interface {
	MarkProcessedMany(ctx context.Context, keys []string, ttl time.Duration) ([]bool, error)
}

// MarkProcessedMany uses d's batch capability when present, else serial
// MarkProcessed (first error aborts — callers fail closed on it). On error
// the returned slice still marks (true) the keys THIS call newly set, so the
// caller releases exactly those — never a pre-existing marker, which would
// let a genuine duplicate through on redelivery. Implementations must honour
// the same contract.
func MarkProcessedMany(ctx context.Context, d DedupStore, keys []string, ttl time.Duration) ([]bool, error) {
	if b, ok := d.(BatchDedupStore); ok {
		return b.MarkProcessedMany(ctx, keys, ttl)
	}
	out := make([]bool, len(keys))
	for i, k := range keys {
		isNew, err := d.MarkProcessed(ctx, k, ttl)
		if err != nil {
			return out, err
		}
		out[i] = isNew
	}
	return out, nil
}

// IdempotentHandler wraps a Handler with deduplication logic.
// If a message has already been processed (based on message ID),
// it is acked silently without calling the inner handler.
func IdempotentHandler(inner Handler, dedup DedupStore, log *slog.Logger, ttl time.Duration) Handler {
	return func(ctx context.Context, msg *Message) error {
		dedupKey := fmt.Sprintf("dedup:%s:%s", msg.Subject, msg.MessageID)

		// Atomic check-and-set: returns true if key was NEW (not duplicate)
		isNew, err := dedup.MarkProcessed(ctx, dedupKey, ttl)
		if err != nil {
			// Redis error - NAK, retry later (don't process without dedup guarantee)
			log.Error("dedup check failed, naking message",
				"subject", msg.Subject,
				"message_id", msg.MessageID,
				"error", err,
			)
			return msg.Nak()
		}

		if !isNew {
			// Duplicate - already processed, ack silently
			log.Debug("duplicate message skipped",
				"subject", msg.Subject,
				"message_id", msg.MessageID,
			)
			return msg.Ack()
		}

		// Process the event
		if err := inner(ctx, msg); err != nil {
			// Processing failed - remove dedup key so retry works
			if unmarkErr := dedup.UnmarkProcessed(ctx, dedupKey); unmarkErr != nil {
				log.Error("failed to unmark dedup key after processing failure",
					"key", dedupKey,
					"error", unmarkErr,
				)
			}
			// NAK - NATS will redeliver
			return msg.Nak()
		}

		// Success - ack
		return msg.Ack()
	}
}
