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

// Handler processes a single event message.
type Handler func(ctx context.Context, msg *Message) error

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
