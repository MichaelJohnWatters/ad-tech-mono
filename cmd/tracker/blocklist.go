package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/fraud"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache/warm"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
)

// startFraudBlocklistCache loads fraud_blocklists into a warm cache and
// pushes each snapshot into the checker via ReplaceBlocklists, so IP/UA
// blocks become DB-driven (manageable + auditable) instead of code-only.
// Refreshes on the cache.warm.fraud_rules poll interval and instantly on
// the adtech.cache.invalidate.fraud-rules NATS subject.
//
// Returns nil when database.url is unset — the checker keeps its hardcoded
// bot patterns and datacenter ranges, so the tracker still does useful
// fraud filtering with no DB (consistent with the platform's fail-open
// boot story).
func startFraudBlocklistCache(cfg *config.Config, log *slog.Logger, bus events.EventBus, checker *fraud.RealTimeChecker) *warm.Cache[fraud.BlocklistEntry] {
	dbURL := cfg.Get("database.url", "")
	if dbURL == "" {
		log.Warn("database.url not set, fraud blocklist cache disabled (hardcoded patterns only)")
		return nil
	}
	pollInterval := cfg.GetDuration("cache.warm.fraud_rules.poll_interval", 60*time.Second)

	loader := &warm.RetryingLoader[fraud.BlocklistEntry]{
		Log:   log,
		KeyFn: func(e fraud.BlocklistEntry) string { return e.Type + ":" + e.Value },
		Construct: func() (warm.Loader[fraud.BlocklistEntry], error) {
			store, err := postgres.New(postgres.Config{PrimaryURL: dbURL, MaxOpenConns: 3, MaxIdleConns: 1, ConnMaxLifetime: 5 * time.Minute})
			if err != nil {
				return nil, fmt.Errorf("postgres connect: %w", err)
			}
			return &postgres.BlocklistLoader{Store: store}, nil
		},
	}
	c := warm.New(warm.Config[fraud.BlocklistEntry]{
		Name:              "fraud_rules",
		Loader:            loader,
		Clock:             clock.Real{},
		Bus:               bus,
		InvalidateSubject: events.SubjectCacheInvalidateFraudRules,
		PollInterval:      pollInterval,
		Log:               log,
		OnRefresh: func(_ context.Context, entries []fraud.BlocklistEntry) {
			checker.ReplaceBlocklists(entries)
		},
	})
	if err := c.Start(context.Background()); err != nil {
		log.Error("fraud blocklist cache initial load failed", "error", err)
	}
	return c
}
