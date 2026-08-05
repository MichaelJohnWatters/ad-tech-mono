// Package events provides the EventBus abstraction for async messaging.
//
// Services never import NATS directly - they use this interface.
// The JetStream implementation lives in events/nats/.
// Swapping to Kafka means adding events/kafka/ and changing a config value.
//
// All consumers use the IdempotentConsumer wrapper by default to prevent
// double-processing from NATS at-least-once delivery.
//
// Usage:
//
//	bus := natsbus.New(natsConn, redisClient, logger)
//	bus.Publish(ctx, "adtech.events.impression", &ImpressionEvent{...})
//	bus.Subscribe(ctx, "adtech.events.impression", handler)
package events

import (
	"context"
	"fmt"
)

// EventBus is the core abstraction for async event publishing and consumption.
// Implementations: nats/ (JetStream), kafka/ (future).
type EventBus interface {
	// Publish sends a message to the given subject.
	// The message is serialized as protobuf bytes.
	Publish(ctx context.Context, subject string, data []byte) error

	// Subscribe registers a handler for messages on the given subject.
	// The handler is wrapped with idempotent consumer logic by default.
	// Consumer group is derived from the service name.
	Subscribe(ctx context.Context, subject string, group string, handler Handler) error

	// Close gracefully shuts down the event bus connection.
	Close() error
}

// PublisherWithID is an optional capability: buses that support
// publisher-side exactly-once semantics (server-side dedup by message ID
// within the stream's duplicate window) implement it. JetStream republishes
// after an ambiguous publish ack (client retry on timeout) create a SECOND
// stream entry with a new sequence — consumer-side sequence dedup cannot
// see it, which double-counted 251 impressions in the 2026-07-18 hour run.
// A stable business ID lets the SERVER drop the republish instead.
type PublisherWithID interface {
	PublishWithID(ctx context.Context, subject, msgID string, data []byte) error
}

// PublishDedup publishes with a stable message ID when the bus supports it,
// falling back to plain Publish (memory bus, fakes) otherwise. msgID must be
// stable across retries of the SAME logical event and unique across distinct
// events — derive it from the trace ID plus whatever disambiguates the event
// within a trace (subject, endpoint, conversion type…).
func PublishDedup(ctx context.Context, bus EventBus, subject, msgID string, data []byte) error {
	// Nil-bus guard at the single choke point: a service that booted before
	// NATS latches bus=nil, and a nil interface here SIGSEGVed all three
	// tracker pods on the 2026-08-05 fresh-disk boot (typed video/audio
	// publishes don't pre-check the bus the way the tracker's beacon path
	// does). An error return lets the spool absorb the event instead.
	if bus == nil {
		return fmt.Errorf("publish %s: event bus not connected", subject)
	}
	if p, ok := bus.(PublisherWithID); ok && msgID != "" {
		return p.PublishWithID(ctx, subject, msgID, data)
	}
	return bus.Publish(ctx, subject, data)
}

// Handler processes a single event message.
type Handler func(ctx context.Context, msg *Message) error

// BatchHandler processes a slice of messages delivered together (one
// JetStream fetch). The handler owns ack/nak of each message — it decides
// per message whether to Ack (processed / poison / duplicate) or Nak
// (transient failure, redeliver). Returning an error is advisory (logged);
// it does not ack/nak on the handler's behalf.
type BatchHandler func(ctx context.Context, msgs []*Message) error

// BatchSubscriber is the optional bulk-consume capability. Implemented by
// the NATS bus (a JetStream fetch already returns N messages at once) so a
// consumer can insert N rows as one atomic block instead of N single-row
// writes. Discovered by type assertion on EventBus; the memory bus
// implements a trivial size-1 version for tests.
type BatchSubscriber interface {
	SubscribeBatch(ctx context.Context, subject, group string, handler BatchHandler) error
}

// BroadcastSubscriber is the optional per-pod fan-out capability: every
// subscribing pod gets its OWN copy of each message (broadcast), NOT a shared
// queue group. Used for cache invalidation, where each replica must refresh
// its own in-process snapshot.
//
// Crucially, the underlying consumer is EPHEMERAL with an inactivity timeout —
// when the pod dies, NATS auto-deletes the consumer. This is what a per-pod
// group MUST NOT be built from `Subscribe` (which creates a permanent durable
// keyed by the pod name): those durables never get cleaned up, so every pod
// that ever existed leaks a consumer forever. That leak wedged the JetStream
// meta layer on 2026-07-25 (1039 orphaned cache-invalidate consumers → 2m+
// CONSUMER.CREATE latency). Ephemeral + InactiveThreshold is the fix.
//
// Delivery is best-effort (DeliverNew, no redelivery guarantee): a missed
// invalidate only means a stale cache until the warm-cache poll backstop
// catches up, so the guarantees of a durable consumer aren't needed here.
//
// Discovered by type assertion on EventBus; callers fall back to Subscribe
// (with a per-pod group) when the bus doesn't implement it — that keeps the
// memory bus / fakes working, at the cost of the old durable behaviour in
// tests (which never run long enough to leak).
type BroadcastSubscriber interface {
	SubscribeBroadcast(ctx context.Context, subject, name string, handler Handler) error
}

// SubscribeBroadcast subscribes with per-pod ephemeral fan-out when the bus
// supports it, else falls back to a per-pod durable group via Subscribe. name
// must be unique per pod (e.g. cacheName + "-" + podid.Replica()).
func SubscribeBroadcast(ctx context.Context, bus EventBus, subject, name string, handler Handler) error {
	if b, ok := bus.(BroadcastSubscriber); ok {
		return b.SubscribeBroadcast(ctx, subject, name, handler)
	}
	return bus.Subscribe(ctx, subject, name, handler)
}

// Message represents a received event.
type Message struct {
	Subject   string
	Data      []byte
	TraceID   string
	MessageID string // unique message ID for dedup

	// ack/nak are set by the implementation
	ackFn func() error
	nakFn func() error
}

// Ack acknowledges successful processing. Message won't be redelivered.
func (m *Message) Ack() error {
	if m.ackFn != nil {
		return m.ackFn()
	}
	return nil
}

// Nak signals processing failure. Message will be redelivered with backoff.
func (m *Message) Nak() error {
	if m.nakFn != nil {
		return m.nakFn()
	}
	return nil
}

// NewMessage creates a Message with ack/nak functions.
// Used by EventBus implementations.
func NewMessage(subject string, data []byte, traceID, messageID string, ack, nak func() error) *Message {
	return &Message{
		Subject:   subject,
		Data:      data,
		TraceID:   traceID,
		MessageID: messageID,
		ackFn:     ack,
		nakFn:     nak,
	}
}
