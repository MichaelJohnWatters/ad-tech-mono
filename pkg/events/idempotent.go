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
