package main

// partners.go — hot-path integration of the partner registry (#112). An 'active'
// DSP partner is a real bidder in the auction: its endpoint is merged into the
// fan-out list. To respect the hot-path iron rule (no per-auction network I/O),
// the auction reads an in-process SNAPSHOT that a background ticker keeps warm
// from Postgres — never a query per bid request. Gated by
// exchange.partner_registry_enabled (checked at read time, so flipping it is live).

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/partner"
	partnerpg "github.com/MichaelJohnWatters/ad-tech-mono/pkg/partner/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
)

// partnerEndpoints is the warm, in-process snapshot of active DSP-partner fan-out
// endpoints. It implements warm.Refreshable so the debug cache-refresh hook can
// force a reload (e2e / ops).
type partnerEndpoints struct {
	store partner.Store
	log   *slog.Logger
	snap  atomic.Value // []string, always set
}

func (p *partnerEndpoints) Name() string { return "partner-endpoints" }

// Refresh reloads the active DSP-partner endpoint set into the snapshot.
func (p *partnerEndpoints) Refresh(ctx context.Context) (int, error) {
	eps, err := p.store.ActiveDSPEndpoints(ctx)
	if err != nil {
		return 0, err
	}
	if eps == nil {
		eps = []string{}
	}
	p.snap.Store(eps)
	return len(eps), nil
}

// Snapshot returns the current active-partner endpoint set (never nil).
func (p *partnerEndpoints) Snapshot() []string {
	if v, ok := p.snap.Load().([]string); ok {
		return v
	}
	return []string{}
}

// startPartnerEndpoints connects, does an initial load, and starts the background
// refresh. Returns nil when there's no DB (the fan-out then uses only config).
func startPartnerEndpoints(ctx context.Context, cfg *config.Config, log *slog.Logger) *partnerEndpoints {
	dbURL := cfg.Get(keys.Database.URL.Key(), "")
	if dbURL == "" {
		return nil
	}
	store, err := postgres.New(postgres.Config{PrimaryURL: dbURL, MaxOpenConns: 3, MaxIdleConns: 1, ConnMaxLifetime: 5 * time.Minute})
	if err != nil {
		log.Warn("partner registry: db connect failed; active partners won't fan out", "error", err)
		return nil
	}
	pe := &partnerEndpoints{store: partnerpg.New(store.Primary()), log: log}
	pe.snap.Store([]string{})
	if n, err := pe.Refresh(ctx); err != nil {
		log.Warn("partner registry: initial load failed (will retry on the poll)", "error", err)
	} else {
		log.Info("partner registry warm cache loaded", "active_dsp_partners", n)
	}
	interval := keys.Exchange.PartnerRefreshInterval.Get(cfg)
	if interval <= 0 {
		interval = 30 * time.Second
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if _, err := pe.Refresh(context.Background()); err != nil {
					log.Warn("partner registry refresh failed", "error", err)
				}
			}
		}
	}()
	return pe
}
