package main

import (
	"io"
	"log/slog"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
)

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// The memory backend is the default and the one the e2e harness runs.
// If this ever stops returning a *MemoryStore, the /debug read-back
// endpoints (and the whole e2e suite that depends on them) break — so
// pin it.
func TestSelectAnalyticsStore_DefaultIsMemory(t *testing.T) {
	cfg := config.Load()
	store := selectAnalyticsStore(cfg, quietLog())
	if _, ok := store.(*analytics.MemoryStore); !ok {
		t.Fatalf("default backend = %T, want *analytics.MemoryStore", store)
	}
}

func TestSelectAnalyticsStore_ExplicitMemory(t *testing.T) {
	cfg := config.Load()
	cfg.SetLive("reporting.analytics_backend", "memory")
	store := selectAnalyticsStore(cfg, quietLog())
	if _, ok := store.(*analytics.MemoryStore); !ok {
		t.Fatalf("memory backend = %T, want *analytics.MemoryStore", store)
	}
}
