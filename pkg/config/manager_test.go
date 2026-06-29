package config

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
)

func TestManager_SetAndGet(t *testing.T) {
	cfg := Load()
	log := logger.New("config-test")
	mgr := NewManager(cfg, log)

	mgr.Set(context.Background(), "exchange.bid_timeout", "200ms")

	val := cfg.Get("exchange.bid_timeout", "100ms")
	if val != "200ms" {
		t.Errorf("got %s, want 200ms", val)
	}
}

func TestManager_OnChange(t *testing.T) {
	cfg := Load()
	log := logger.New("config-test")
	mgr := NewManager(cfg, log)

	// OnChange callbacks fire in goroutines (Manager.applyChanges); guard
	// the captured state with a Mutex so -race stays clean.
	var (
		mu                sync.Mutex
		changed           bool
		gotOld, gotNew    string
	)
	mgr.OnChange("test.key", func(_, old, new_ string) {
		mu.Lock()
		changed = true
		gotOld = old
		gotNew = new_
		mu.Unlock()
	})

	mgr.Set(context.Background(), "test.key", "value1")
	waitFor(t, "first change", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return changed && gotNew == "value1"
	})

	mu.Lock()
	changed = false
	mu.Unlock()
	mgr.Set(context.Background(), "test.key", "value2")
	waitFor(t, "second change", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return changed && gotOld == "value1" && gotNew == "value2"
	})
}

func TestManager_Polling(t *testing.T) {
	cfg := Load()
	log := logger.New("config-test")
	mgr := NewManager(cfg, log)

	source := NewMemorySource(map[string]string{"poll.key": "initial"})
	mgr.SetSource(source)
	mgr.SetPollInterval(50 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)

	waitFor(t, "initial poll loaded", func() bool { return cfg.Get("poll.key", "") == "initial" })

	source.Update(context.Background(), "poll.key", "updated")
	waitFor(t, "poll picked up update", func() bool { return cfg.Get("poll.key", "") == "updated" })
}

func TestManager_History(t *testing.T) {
	cfg := Load()
	log := logger.New("config-test")
	mgr := NewManager(cfg, log)

	mgr.Set(context.Background(), "a", "1")
	mgr.Set(context.Background(), "b", "2")
	mgr.Set(context.Background(), "a", "3")

	history := mgr.History(10)
	if len(history) != 3 {
		t.Errorf("history = %d, want 3", len(history))
	}
}

func TestManager_Remove(t *testing.T) {
	cfg := Load()
	log := logger.New("config-test")
	mgr := NewManager(cfg, log)

	mgr.Set(context.Background(), "del.key", "value")
	if cfg.Get("del.key", "") != "value" {
		t.Fatal("set failed")
	}

	mgr.Remove(context.Background(), "del.key")
	if cfg.Get("del.key", "default") != "default" {
		t.Error("expected key to be removed, falling back to default")
	}
}

func TestManager_NoChangeNoCallback(t *testing.T) {
	cfg := Load()
	log := logger.New("config-test")
	mgr := NewManager(cfg, log)

	var callCount atomic.Int32
	mgr.OnChange("stable", func(_, _, _ string) { callCount.Add(1) })

	mgr.Set(context.Background(), "stable", "val")
	waitFor(t, "first call fired", func() bool { return callCount.Load() == 1 })

	// Set same value again — applyChanges sees oldVal == newVal and does not
	// fire the callback. Give any spurious goroutine ample time to run.
	mgr.Set(context.Background(), "stable", "val")
	time.Sleep(50 * time.Millisecond)
	if got := callCount.Load(); got != 1 {
		t.Errorf("expected still 1 call (no change), got %d", got)
	}
}
