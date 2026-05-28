package lifecycle_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
)

func TestShutdown_RunsHooksInOrder(t *testing.T) {
	var buf bytes.Buffer
	log := logger.NewWithWriter("test", &buf)
	lc := lifecycle.New(log)

	var order []string
	lc.OnShutdown("first", func(ctx context.Context) error {
		order = append(order, "first")
		return nil
	})
	lc.OnShutdown("second", func(ctx context.Context) error {
		order = append(order, "second")
		return nil
	})
	lc.OnShutdown("third", func(ctx context.Context) error {
		order = append(order, "third")
		return nil
	})

	err := lc.Shutdown(5 * time.Second)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if len(order) != 3 || order[0] != "first" || order[1] != "second" || order[2] != "third" {
		t.Errorf("hooks ran in wrong order: %v", order)
	}
}

func TestShutdown_ReturnsErrorOnFailure(t *testing.T) {
	var buf bytes.Buffer
	log := logger.NewWithWriter("test", &buf)
	lc := lifecycle.New(log)

	lc.OnShutdown("ok-hook", func(ctx context.Context) error {
		return nil
	})
	lc.OnShutdown("fail-hook", func(ctx context.Context) error {
		return errors.New("flush failed")
	})

	err := lc.Shutdown(5 * time.Second)
	if err == nil {
		t.Error("expected error from failed hook")
	}
}

func TestShutdown_RespectsGracePeriod(t *testing.T) {
	var buf bytes.Buffer
	log := logger.NewWithWriter("test", &buf)
	lc := lifecycle.New(log)

	lc.OnShutdown("slow-hook", func(ctx context.Context) error {
		select {
		case <-time.After(10 * time.Second):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})

	err := lc.Shutdown(100 * time.Millisecond)
	if err == nil {
		t.Error("expected error from context deadline exceeded")
	}
}

func TestShutdown_NoHooks(t *testing.T) {
	var buf bytes.Buffer
	log := logger.NewWithWriter("test", &buf)
	lc := lifecycle.New(log)

	err := lc.Shutdown(5 * time.Second)
	if err != nil {
		t.Errorf("unexpected error with no hooks: %v", err)
	}
}
