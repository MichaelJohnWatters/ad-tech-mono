package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache/warm"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/fraud"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
)

// startAdsTxtCache loads ads_txt_cache into a warm cache and repopulates the
// in-memory AdsTxtCache (which the auction handler queries via IsAuthorised)
// on every refresh. Only 'valid' records — those with parsed entries — are
// loaded; 'missing'/'error' domains are left absent so IsAuthorised returns
// no_ads_txt (unverifiable → allowed) rather than not_listed (rejected).
//
// Returns nil when database.url is unset; enforcement then no-ops (the gate
// treats an empty cache as unverifiable).
func startAdsTxtCache(cfg *config.Config, log *slog.Logger, bus events.EventBus, adsTxt *fraud.AdsTxtCache) *warm.Cache[fraud.AdsTxtRecord] {
	dbURL := cfg.Get("database.url", "")
	if dbURL == "" {
		log.Warn("database.url not set, ads.txt cache disabled (enforcement no-ops)")
		return nil
	}
	pollInterval := cfg.GetDuration("cache.warm.ads_txt.poll_interval", 300*time.Second)

	loader := &warm.RetryingLoader[fraud.AdsTxtRecord]{
		Log:   log,
		KeyFn: func(r fraud.AdsTxtRecord) string { return r.Domain },
		Construct: func() (warm.Loader[fraud.AdsTxtRecord], error) {
			store, err := postgres.New(postgres.Config{PrimaryURL: dbURL, MaxOpenConns: 3, MaxIdleConns: 1, ConnMaxLifetime: 5 * time.Minute})
			if err != nil {
				return nil, fmt.Errorf("postgres connect: %w", err)
			}
			return &postgres.AdsTxtLoader{Store: store}, nil
		},
	}
	c := warm.New(warm.Config[fraud.AdsTxtRecord]{
		Name:              "ads_txt",
		Loader:            loader,
		Clock:             clock.Real{},
		Bus:               bus,
		InvalidateSubject: events.SubjectCacheInvalidateAdsTxt,
		PollInterval:      pollInterval,
		Log:               log,
		OnRefresh: func(_ context.Context, recs []fraud.AdsTxtRecord) {
			for _, r := range recs {
				if r.Status == "valid" {
					adsTxt.Update(r.Domain, r.Entries)
				}
			}
		},
	})
	if err := c.Start(context.Background()); err != nil {
		log.Error("ads.txt cache initial load failed", "error", err)
	}
	return c
}

// adsTxtGateFn returns a per-auction check: given the publisher domain, is
// our platform an authorised seller? Reads the enforcement mode live
// (off|warn|strict, default off) so ops can ratchet without a restart.
//
//   - off:    always allow.
//   - authorised:   allow.
//   - no_ads_txt:   allow (publisher has no ads.txt — can't prove a negative).
//   - not_listed:   strict → reject (no-bid); warn → log + allow.
func adsTxtGateFn(cfg *config.Config, adsTxt *fraud.AdsTxtCache, log *slog.Logger) func(domain string) (bool, string) {
	return func(domain string) (bool, string) {
		mode := strings.ToLower(strings.TrimSpace(cfg.Get("exchange.adstxt_enforcement", "off")))
		if mode == "" || mode == "off" || domain == "" || adsTxt == nil {
			return true, ""
		}
		res := adsTxt.IsAuthorised(domain,
			cfg.Get("exchange.adstxt_seller_domain", ""),
			cfg.Get("exchange.adstxt_seller_id", ""))
		if res.Authorised || res.Status == "no_ads_txt" {
			return true, ""
		}
		// not_listed: the publisher published an ads.txt and we're absent.
		if mode == "strict" {
			return false, "adstxt_not_authorised"
		}
		log.Warn("ads.txt: platform not an authorised seller (warn mode, allowing)", "domain", domain, "status", res.Status)
		return true, ""
	}
}
