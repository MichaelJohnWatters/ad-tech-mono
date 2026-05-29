package config

import (
	"context"
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

	var changed bool
	var gotOld, gotNew string
	mgr.OnChange("test.key", func(key, old, new_ string) {
		changed = true
		gotOld = old
		gotNew = new_
	})

	mgr.Set(context.Background(), "test.key", "value1")
	time.Sleep(50 * time.Millisecond) // callbacks are async

	if !changed {
		t.Error("expected change callback to fire")
	}
	if gotNew != "value1" {
		t.Errorf("new = %s, want value1", gotNew)
	}

	// Change again
	changed = false
	mgr.Set(context.Background(), "test.key", "value2")
	time.Sleep(50 * time.Millisecond)

	if !changed {
		t.Error("expected second change callback")
	}
	if gotOld != "value1" || gotNew != "value2" {
		t.Errorf("old=%s new=%s, want value1/value2", gotOld, gotNew)
	}
}

func TestManager_Polling(t *testing.T) {
	cfg := Load()
	log := logger.New("config-test")
	mgr := NewManager(cfg, log)

	source := NewMemorySource(map[string]string{"poll.key": "initial"})
	mgr.SetSource(source)
	mgr.SetPollInterval(100 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)

	// Initial poll should have loaded the value
	time.Sleep(50 * time.Millisecond)
	val := cfg.Get("poll.key", "")
	if val != "initial" {
		t.Errorf("got %s, want initial", val)
	}

	// Update source, wait for next poll
	source.Update(context.Background(), "poll.key", "updated")
	time.Sleep(200 * time.Millisecond)

	val = cfg.Get("poll.key", "")
	if val != "updated" {
		t.Errorf("got %s, want updated after poll", val)
	}
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

	callCount := 0
	mgr.OnChange("stable", func(_, _, _ string) { callCount++ })

	mgr.Set(context.Background(), "stable", "val")
	time.Sleep(50 * time.Millisecond)
	if callCount != 1 {
		t.Fatalf("expected 1 call, got %d", callCount)
	}

	// Set same value again - should NOT fire callback
	mgr.Set(context.Background(), "stable", "val")
	time.Sleep(50 * time.Millisecond)
	if callCount != 1 {
		t.Errorf("expected still 1 call (no change), got %d", callCount)
	}
}
