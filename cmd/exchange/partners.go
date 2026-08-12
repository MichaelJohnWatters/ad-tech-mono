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

	"github.com/prometheus/client_golang/prometheus"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/partner"
	partnerpg "github.com/MichaelJohnWatters/ad-tech-mono/pkg/partner/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/podid"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
)

// activePartnerGauge tracks how many active DSP partners are currently fanned out
// (money-path observability — alert on a sudden drop to 0 = a DB blip / bad load).
var activePartnerGauge = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: "adtech", Subsystem: "exchange", Name: "active_partner_endpoints",
	Help: "Active DSP-partner endpoints currently in the exchange auction fan-out warm cache.",
})

func init() { prometheus.MustRegister(activePartnerGauge) }

// partnerEndpoints is the warm, in-process snapshot of active DSP-partner fan-out
// endpoints. It implements warm.Refreshable (Name/Refresh) so the debug
// cache-refresh hook can force a reload, and the optional invalidatable
// (PublishInvalidate) so that refresh BROADCASTS to every replica.
type partnerEndpoints struct {
	store   partner.Store
	bus     events.EventBus
	subject string
	log     *slog.Logger
	snap    atomic.Value // []string, always set
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
	activePartnerGauge.Set(float64(len(eps)))
	return len(eps), nil
}

// PublishInvalidate tells the OTHER replicas to refresh too (per-pod ephemeral
// broadcast — a shared group would load-balance the invalidate to one pod).
func (p *partnerEndpoints) PublishInvalidate(ctx context.Context) error {
	if p.bus == nil {
		return nil
	}
	return p.bus.Publish(ctx, p.subject, nil)
}

// Snapshot returns the current active-partner endpoint set (never nil).
func (p *partnerEndpoints) Snapshot() []string {
	if v, ok := p.snap.Load().([]string); ok {
		return v
	}
	return []string{}
}

// subscribe attaches the broadcast handler that refreshes this pod on invalidate.
func (p *partnerEndpoints) subscribe(ctx context.Context, name string) error {
	return events.SubscribeBroadcast(ctx, p.bus, p.subject, name,
		func(context.Context, *events.Message) error {
			if _, err := p.Refresh(context.Background()); err != nil {
				p.log.Warn("partner registry: refresh-on-invalidate failed", "error", err)
			}
			return nil
		})
}

// startPartnerEndpoints connects, does an initial load, subscribes to the
// cross-pod invalidate, and starts the background refresh. Returns nil when
// there's no DB (the fan-out then uses only config).
func startPartnerEndpoints(ctx context.Context, cfg *config.Config, bus events.EventBus, log *slog.Logger) *partnerEndpoints {
	dbURL := cfg.Get(keys.Database.URL.Key(), "")
	if dbURL == "" {
		return nil
	}
	store, err := postgres.New(postgres.Config{PrimaryURL: dbURL, MaxOpenConns: 3, MaxIdleConns: 1, ConnMaxLifetime: 5 * time.Minute})
	if err != nil {
		log.Warn("partner registry: db connect failed; active partners won't fan out", "error", err)
		return nil
	}
	pe := &partnerEndpoints{
		store:   partnerpg.New(store.Primary()),
		bus:     bus,
		subject: events.SubjectCacheInvalidatePartnerEPs,
		log:     log,
	}
	pe.snap.Store([]string{})
	if n, err := pe.Refresh(ctx); err != nil {
		log.Warn("partner registry: initial load failed (will retry on the poll)", "error", err)
	} else {
		log.Info("partner registry warm cache loaded", "active_dsp_partners", n)
	}

	// Cross-pod invalidate subscription — per-replica name, ephemeral, retry-until-
	// stick (the deaf-on-boot rule: a failed first subscribe must self-heal, else a
	// staff 'activate + refresh' only reaches the one pod the debug call hit).
	if bus != nil {
		name := "partner-endpoints-" + podid.Replica()
		if err := pe.subscribe(ctx, name); err != nil {
			log.Warn("partner registry: invalidate subscribe failed, retrying in background", "error", err)
			go func() {
				for {
					select {
					case <-ctx.Done():
						return
					case <-time.After(30 * time.Second):
						if pe.subscribe(ctx, name) == nil {
							log.Info("partner registry: invalidate subscription established after retry")
							return
						}
					}
				}
			}()
		}
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
