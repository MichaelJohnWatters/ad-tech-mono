package events

import (
	"context"
	"fmt"
	"sync"
)

// MemoryBus is an in-process EventBus using Go channels.
// Used for local dev and testing when NATS isn't available.
// Messages are delivered synchronously within the same process.
type MemoryBus struct {
	mu          sync.RWMutex
	subscribers map[string][]subscription
	closed      bool
}

type subscription struct {
	group   string
	handler Handler
}

var _ BatchSubscriber = (*MemoryBus)(nil)

// NewMemoryBus creates an in-process event bus.
func NewMemoryBus() *MemoryBus {
	return &MemoryBus{
		subscribers: make(map[string][]subscription),
	}
}

func (b *MemoryBus) Publish(ctx context.Context, subject string, data []byte) error {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.closed {
		return fmt.Errorf("bus closed")
	}

	subs := b.subscribers[subject]
	if len(subs) == 0 {
		return nil // no subscribers, message dropped (like NATS with no consumers)
	}

	// Deliver to one subscriber per group (like NATS queue groups)
	delivered := make(map[string]bool)
	for _, sub := range subs {
		if delivered[sub.group] {
			continue
		}
		delivered[sub.group] = true

		msg := NewMessage(subject, data, "", fmt.Sprintf("mem-%d", len(data)),
			func() error { return nil },
			func() error { return nil },
		)
		// Deliver synchronously (in-process)
		if err := sub.handler(ctx, msg); err != nil {
			// Handler failed - in NATS this would nak and redeliver
			continue
		}
	}
	return nil
}

func (b *MemoryBus) Subscribe(_ context.Context, subject, group string, handler Handler) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return fmt.Errorf("bus closed")
	}

	b.subscribers[subject] = append(b.subscribers[subject], subscription{
		group:   group,
		handler: handler,
	})
	return nil
}

// SubscribeBatch adapts the in-process bus to events.BatchSubscriber by
// delivering each published message as a one-element batch. Enough for tests
// and the HTTP/dev path; the real batching (a JetStream fetch of N) only
// happens on the NATS bus.
func (b *MemoryBus) SubscribeBatch(_ context.Context, subject, group string, handler BatchHandler) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return fmt.Errorf("bus closed")
	}
	wrapped := func(ctx context.Context, msg *Message) error {
		return handler(ctx, []*Message{msg})
	}
	b.subscribers[subject] = append(b.subscribers[subject], subscription{group: group, handler: wrapped})
	return nil
}

func (b *MemoryBus) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	return nil
}
