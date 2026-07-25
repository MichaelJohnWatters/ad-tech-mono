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

// TestManager_PollRemovalRevertsToDefault — deleting a live config row
// must revert the key on the next poll, not pin the last-known value in
// memory forever. The regression this guards: ops deleted a row and every
// running pod silently kept serving the deleted value until restart.
func TestManager_PollRemovalRevertsToDefault(t *testing.T) {
	cfg := Load()
	mgr := NewManager(cfg, logger.New("config-test"))
	// Register the key's schema like Setup would for a real service, so
	// the removal callback can rebase to the schema default.
	Register("removal-test", []SchemaEntry{{
		Key: "removaltest.knob", Type: "duration", Default: "500ms",
		Tier: TierLive, Service: "removal-test",
	}})
	source := NewMemorySource(map[string]string{"removaltest.knob": "200ms"})
	mgr.SetSource(source)

	var lastNew atomic.Value
	mgr.OnChange("removaltest.knob", func(_, _, newVal string) {
		lastNew.Store(newVal)
	})

	mgr.poll(context.Background())
	if got := cfg.Get("removaltest.knob", "x"); got != "200ms" {
		t.Fatalf("after first poll = %q, want 200ms", got)
	}

	// Row deleted at the source — next poll must clear the live layer.
	if err := source.Delete(context.Background(), "removaltest.knob"); err != nil {
		t.Fatal(err)
	}
	mgr.poll(context.Background())

	// The live layer is gone: reads fall through to the caller default.
	if got := cfg.Get("removaltest.knob", "fallback"); got != "fallback" {
		t.Fatalf("after removal poll = %q, want caller fallback (live layer cleared)", got)
	}

	// Callback rebased to the schema default, parseable by Live* handles.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if v, ok := lastNew.Load().(string); ok && v == "500ms" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("removal callback newVal = %v, want schema default 500ms", lastNew.Load())
}

// TestManager_PartialApplyDoesNotRemove — a single-key API write must
// never be treated as a full snapshot: other live keys stay intact.
func TestManager_PartialApplyDoesNotRemove(t *testing.T) {
	cfg := Load()
	mgr := NewManager(cfg, logger.New("config-test"))
	source := NewMemorySource(map[string]string{
		"exchange.bid_timeout": "200ms",
		"exchange.channel":     "video",
	})
	mgr.SetSource(source)
	mgr.poll(context.Background())

	// API-path partial update of ONE key.
	mgr.applyChanges(map[string]string{"exchange.bid_timeout": "300ms"}, "api")

	if got := cfg.Get("exchange.channel", "x"); got != "video" {
		t.Fatalf("partial apply removed an unrelated live key: channel = %q, want video", got)
	}
}
