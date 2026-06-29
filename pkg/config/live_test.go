package config

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

// liveTestManager builds a Manager backed by an in-memory source so the
// test can drive Set / OnChange paths without Postgres.
func liveTestManager(t *testing.T, seed map[string]string) (*Manager, *Config) {
	t.Helper()
	cfg := Load()
	cfg.SetLiveBatch(seed)
	mgr := NewManager(cfg, slog.Default())
	mgr.SetSource(NewMemorySource(seed))
	return mgr, cfg
}

// waitFor polls f every 5ms for up to 200ms, failing the test if the
// expected condition never holds. OnChange callbacks fire in goroutines
// (Manager.applyChanges) so reads immediately after Set race the swap.
func waitFor(t *testing.T, label string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s: condition not met within 200ms", label)
}

func TestLiveDurationPicksUpChange(t *testing.T) {
	mgr, cfg := liveTestManager(t, map[string]string{
		"test.timeout": "100ms",
	})
	lv := NewLiveDuration(mgr, cfg, "test.timeout", time.Second)
	if got := lv.Value(); got != 100*time.Millisecond {
		t.Fatalf("initial value: got %v, want 100ms", got)
	}

	if err := mgr.Set(context.Background(), "test.timeout", "250ms"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	waitFor(t, "duration swap", func() bool { return lv.Value() == 250*time.Millisecond })
}

func TestLiveDurationIgnoresBadParse(t *testing.T) {
	mgr, cfg := liveTestManager(t, map[string]string{"x": "10s"})
	lv := NewLiveDuration(mgr, cfg, "x", time.Minute)
	if got := lv.Value(); got != 10*time.Second {
		t.Fatalf("initial: got %v", got)
	}
	// Validate gate would normally catch this; here we bypass via direct
	// applyChanges to confirm Live* doesn't crash on garbage.
	mgr.applyChanges(map[string]string{"x": "not-a-duration"}, "test")
	time.Sleep(20 * time.Millisecond) // give the (no-op) callback a chance to run
	if got := lv.Value(); got != 10*time.Second {
		t.Fatalf("bad parse should keep previous: got %v", got)
	}
}

func TestLiveBoolAndIntAndFloat(t *testing.T) {
	mgr, cfg := liveTestManager(t, map[string]string{
		"b": "true", "i": "5", "f": "1.5",
	})
	lb := NewLiveBool(mgr, cfg, "b", false)
	li := NewLiveInt(mgr, cfg, "i", 0)
	lf := NewLiveFloat(mgr, cfg, "f", 0)
	if !lb.Value() || li.Value() != 5 || lf.Value() != 1.5 {
		t.Fatalf("initial: bool=%v int=%d float=%v", lb.Value(), li.Value(), lf.Value())
	}

	if err := mgr.Set(context.Background(), "b", "false"); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Set(context.Background(), "i", "12"); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Set(context.Background(), "f", "3.14"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "bool/int/float swap", func() bool {
		return !lb.Value() && li.Value() == 12 && lf.Value() == 3.14
	})
}

func TestLiveStringChange(t *testing.T) {
	mgr, cfg := liveTestManager(t, map[string]string{"s": "alpha"})
	ls := NewLiveString(mgr, cfg, "s", "default")
	if got := ls.Value(); got != "alpha" {
		t.Fatalf("initial: got %q", got)
	}
	if err := mgr.Set(context.Background(), "s", "beta"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "string swap", func() bool { return ls.Value() == "beta" })
}
