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
