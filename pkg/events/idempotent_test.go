package events_test

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
)

// memoryDedupStore is an in-memory DedupStore for testing (no Redis needed).
type memoryDedupStore struct {
	mu   sync.Mutex
	seen map[string]bool
}

func newMemoryDedup() *memoryDedupStore {
	return &memoryDedupStore{seen: make(map[string]bool)}
}

func (m *memoryDedupStore) MarkProcessed(_ context.Context, key string, _ time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.seen[key] {
		return false, nil // duplicate
	}
	m.seen[key] = true
	return true, nil // new
}

func (m *memoryDedupStore) UnmarkProcessed(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.seen, key)
	return nil
}

// failingDedupStore always returns an error (simulates Redis failure).
type failingDedupStore struct{}

func (f *failingDedupStore) MarkProcessed(_ context.Context, _ string, _ time.Duration) (bool, error) {
	return false, errors.New("redis connection refused")
}

func (f *failingDedupStore) UnmarkProcessed(_ context.Context, _ string) error {
	return errors.New("redis connection refused")
}

func TestIdempotentHandler_ProcessesNewMessage(t *testing.T) {
	dedup := newMemoryDedup()
	var buf bytes.Buffer
	log := logger.NewWithWriter("test", &buf)

	processed := false
	inner := func(ctx context.Context, msg *events.Message) error {
		processed = true
		return nil
	}

	handler := events.IdempotentHandler(inner, dedup, log, 24*time.Hour)

	var acked bool
	msg := events.NewMessage("adtech.events.impression", []byte("data"), "trace-1", "msg-1",
		func() error { acked = true; return nil },
		func() error { return nil },
	)

	err := handler(context.Background(), msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !processed {
		t.Error("inner handler was not called for new message")
	}
	if !acked {
		t.Error("message was not acked")
	}
}

func TestIdempotentHandler_SkipsDuplicate(t *testing.T) {
	dedup := newMemoryDedup()
	var buf bytes.Buffer
	log := logger.NewWithWriter("test", &buf)

	processCount := 0
	inner := func(ctx context.Context, msg *events.Message) error {
		processCount++
		return nil
	}

	handler := events.IdempotentHandler(inner, dedup, log, 24*time.Hour)

	makeMsg := func() *events.Message {
		return events.NewMessage("adtech.events.impression", []byte("data"), "trace-1", "msg-1",
			func() error { return nil },
			func() error { return nil },
		)
	}

	// First call: should process
	handler(context.Background(), makeMsg())
	if processCount != 1 {
		t.Errorf("process count = %d, want 1", processCount)
	}

	// Second call: same message ID, should skip
	handler(context.Background(), makeMsg())
	if processCount != 1 {
		t.Errorf("process count = %d, want 1 (duplicate should be skipped)", processCount)
	}
}

func TestIdempotentHandler_RetryOnProcessingFailure(t *testing.T) {
	dedup := newMemoryDedup()
	var buf bytes.Buffer
	log := logger.NewWithWriter("test", &buf)

	callCount := 0
	inner := func(ctx context.Context, msg *events.Message) error {
		callCount++
		if callCount == 1 {
			return errors.New("transient error")
		}
		return nil
	}

	handler := events.IdempotentHandler(inner, dedup, log, 24*time.Hour)

	var naked bool
	makeMsg := func() *events.Message {
		return events.NewMessage("adtech.events.impression", []byte("data"), "trace-1", "msg-retry",
			func() error { return nil },
			func() error { naked = true; return nil },
		)
	}

	// First call: processing fails, should nak
	handler(context.Background(), makeMsg())
	if !naked {
		t.Error("message was not naked on processing failure")
	}
	if callCount != 1 {
		t.Errorf("call count = %d, want 1", callCount)
	}

	// Second call (retry): should succeed because dedup key was cleared
	naked = false
	handler(context.Background(), makeMsg())
	if callCount != 2 {
		t.Errorf("call count = %d, want 2 (retry should process)", callCount)
	}
}

func TestIdempotentHandler_NakOnDedupFailure(t *testing.T) {
	dedup := &failingDedupStore{}
	var buf bytes.Buffer
	log := logger.NewWithWriter("test", &buf)

	processed := false
	inner := func(ctx context.Context, msg *events.Message) error {
		processed = true
		return nil
	}

	handler := events.IdempotentHandler(inner, dedup, log, 24*time.Hour)

	var naked bool
	msg := events.NewMessage("adtech.events.impression", []byte("data"), "trace-1", "msg-1",
		func() error { return nil },
		func() error { naked = true; return nil },
	)

	handler(context.Background(), msg)

	if processed {
		t.Error("inner handler should NOT be called when dedup store fails")
	}
	if !naked {
		t.Error("message should be naked when dedup store fails")
	}
}
