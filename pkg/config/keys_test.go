package config

import (
	"testing"
	"time"
)

func TestKeySetBuildsSchemaEntries(t *testing.T) {
	s := NewKeySet("testsvc")
	s.String("testsvc.name", "hello", TierStatic, "a string", Since("v1.2"))
	s.Bool("testsvc.enabled", "true", TierLive, "a bool")
	s.Int("testsvc.count", "7", TierLive, "an int")
	s.Float("testsvc.ratio", "0.25", TierLive, "a float")
	s.Duration("testsvc.timeout", "300s", TierStatic, "a duration", OwnedBy("other"))

	entries := s.Entries()
	if len(entries) != 5 {
		t.Fatalf("expected 5 entries, got %d", len(entries))
	}
	byKey := map[string]SchemaEntry{}
	for _, e := range entries {
		byKey[e.Key] = e
	}

	name := byKey["testsvc.name"]
	if name.Type != "string" || name.Tier != TierStatic || name.Default != "hello" || name.Since != "v1.2" || name.Service != "testsvc" {
		t.Errorf("string entry wrong: %+v", name)
	}
	// The duration default string must be preserved byte-identically —
	// "300s" must NOT re-render as "5m0s" (would churn seeded Postgres rows).
	timeout := byKey["testsvc.timeout"]
	if timeout.Default != "300s" {
		t.Errorf("duration default churned: %q", timeout.Default)
	}
	if timeout.Service != "other" {
		t.Errorf("OwnedBy not applied: %q", timeout.Service)
	}
}

func TestTypedKeyGet(t *testing.T) {
	s := NewKeySet("testsvc")
	str := s.String("testsvc.name", "hello", TierStatic, "")
	b := s.Bool("testsvc.enabled", "true", TierLive, "")
	i := s.Int("testsvc.count", "7", TierLive, "")
	f := s.Float("testsvc.ratio", "0.25", TierLive, "")
	d := s.Duration("testsvc.timeout", "300s", TierStatic, "")

	cfg := Load()

	// Defaults when nothing is set.
	if got := str.Get(cfg); got != "hello" {
		t.Errorf("string default: %q", got)
	}
	if got := b.Get(cfg); got != true {
		t.Errorf("bool default: %v", got)
	}
	if got := i.Get(cfg); got != 7 {
		t.Errorf("int default: %d", got)
	}
	if got := f.Get(cfg); got != 0.25 {
		t.Errorf("float default: %v", got)
	}
	if got := d.Get(cfg); got != 5*time.Minute {
		t.Errorf("duration default: %v", got)
	}

	// Live values win.
	cfg.SetLive("testsvc.count", "42")
	cfg.SetLive("testsvc.timeout", "1h")
	if got := i.Get(cfg); got != 42 {
		t.Errorf("int live: %d", got)
	}
	if got := d.Get(cfg); got != time.Hour {
		t.Errorf("duration live: %v", got)
	}
}

func TestRawKeys(t *testing.T) {
	cfg := Load()
	d := RawDuration("testsvc.raw_timeout", 90*time.Second)
	if got := d.Get(cfg); got != 90*time.Second {
		t.Errorf("raw duration default: %v", got)
	}
	if d.Key() != "testsvc.raw_timeout" {
		t.Errorf("raw key name: %q", d.Key())
	}
	cfg.SetLive("testsvc.raw_timeout", "10s")
	if got := d.Get(cfg); got != 10*time.Second {
		t.Errorf("raw duration live: %v", got)
	}
}

func TestKeySetPanics(t *testing.T) {
	assertPanics := func(name string, fn func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Errorf("%s: expected panic", name)
			}
		}()
		fn()
	}
	assertPanics("bad duration default", func() {
		NewKeySet("x").Duration("x.t", "nonsense", TierStatic, "")
	})
	assertPanics("bad int default", func() {
		NewKeySet("x").Int("x.n", "1.5", TierStatic, "")
	})
	assertPanics("duplicate key", func() {
		s := NewKeySet("x")
		s.Bool("x.flag", "true", TierLive, "")
		s.Bool("x.flag", "false", TierLive, "")
	})
}
