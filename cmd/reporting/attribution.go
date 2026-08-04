package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
)

// identityResolver expands one id to the platform ids linked to it (either
// direction), via the Postgres identity graph, following only edges at/above a
// confidence floor and capped at `limit` — because a wrong/poisoned link here
// mis-attributes real CPA billing. Optional: nil means same-id-only matching (no
// cross-device / cross-publisher resolution).
type identityResolver interface {
	ResolveIdentityConfident(ctx context.Context, id string, minConfidence float64, limit int) ([]string, error)
}

// lazyIdentityResolver opens the identity-graph Postgres store on FIRST use and
// retries a failed open on every later call, instead of pinging once at boot.
// Boot-latch doctrine: reporting used to construct the store at startup and,
// on any transient failure (fresh-install DNS not up yet, DB briefly
// unavailable), permanently disable cross-device attribution while the pod
// reported ready. A resolve while the DB is down returns an error, which the
// attributor already degrades to same-id matching (WARN) — identical behaviour
// to a nil resolver, but it heals the moment Postgres is reachable.
type lazyIdentityResolver struct {
	url string
	lc  *lifecycle.Lifecycle
	log *slog.Logger

	mu sync.Mutex
	pg *postgres.Store
}

func (l *lazyIdentityResolver) store() (*postgres.Store, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pg != nil {
		return l.pg, nil
	}
	pg, err := postgres.New(postgres.Config{PrimaryURL: l.url, MaxOpenConns: 3, MaxIdleConns: 1, ConnMaxLifetime: 5 * time.Minute})
	if err != nil {
		return nil, err
	}
	l.pg = pg
	l.lc.OnShutdown("attribution-identity-db", func(_ context.Context) error { return pg.Close() })
	l.log.Info("view-through cross-device resolver connected")
	return pg, nil
}

func (l *lazyIdentityResolver) ResolveIdentityConfident(ctx context.Context, id string, minConfidence float64, limit int) ([]string, error) {
	pg, err := l.store()
	if err != nil {
		return nil, err
	}
	return pg.ResolveIdentityConfident(ctx, id, minConfidence, limit)
}

// viewThroughAttributor credits a click-less conversion to the most-recent
// qualifying prior ad exposure — last-touch view-through. It resolves the
// advertiser-side visitor id (Conversion.UserID) to the platform user set via
// the identity graph, then asks the analytics store for that set's viewable
// impressions within the view-through window (scoped to the advertiser account,
// and to the campaign when the conversion names one).
//
// Deliberately inline in reporting (not a separate service): conversions are
// low-volume, and reporting already owns the analytics store + billing settle.
// If conversion volume grows, this is the seam to lift into cmd/attribution-consumer.
type viewThroughAttributor struct {
	reader    analytics.ViewThroughReader
	writer    analytics.AttributionWriter // may be nil (no multi-touch chain capture)
	resolver  identityResolver            // may be nil (no cross-device)
	overrides *pgAttributionConfigSource  // may be nil (global config only)
	cfg       *config.Config
	log       *slog.Logger
}

func newViewThroughAttributor(reader analytics.ViewThroughReader, writer analytics.AttributionWriter, resolver identityResolver, overrides *pgAttributionConfigSource, cfg *config.Config, log *slog.Logger) *viewThroughAttributor {
	return &viewThroughAttributor{reader: reader, writer: writer, resolver: resolver, overrides: overrides, cfg: cfg, log: log}
}

func (a *viewThroughAttributor) enabled() bool {
	return a != nil && keys.Attribution.Enabled.Get(a.cfg)
}

// attribute stamps AttributedTraceID + AttributionType="view_through" on e when
// it finds a qualifying prior exposure, and reports whether it did. No-op when
// disabled, already click-through attributed, or the conversion carries no
// visitor id / account. Mutating e before the conversion is inserted means the
// stored row records the linkage and the settle credits the right exposure.
func (a *viewThroughAttributor) attribute(ctx context.Context, e *analytics.ConversionEvent) bool {
	if a == nil || a.reader == nil || !a.enabled() {
		return false
	}
	// Needs a visitor id + account to resolve/scope. A click-through conversion
	// (ctid already stamped by the tracker) still comes through here so its
	// ASSISTING exposures get recorded in the multi-touch chain; only its
	// last-touch is fixed (the click).
	if e.UserID == "" || e.AccountID == "" {
		return false
	}
	clickThrough := e.AttributedTraceID != ""

	users := a.resolveUsers(ctx, e.UserID)
	base := e.Timestamp
	if base.IsZero() {
		base = time.Now()
	}
	windowHrs := keys.Attribution.ViewThroughWindowHrs.Get(a.cfg)
	requireViewable := keys.Attribution.RequireViewability.Get(a.cfg)
	// Per-line-item overrides (gap G5): a campaign can tighten the window /
	// viewability over the global defaults. Only applies when the conversion names
	// its campaign up front (carries cid, or click-through) — a bare view-through
	// pixel doesn't know the campaign until AFTER the lookback, so globals apply.
	overrideApplied := false
	if a.overrides != nil && e.CampaignID != "" {
		if ov := a.overrides.Get(ctx, e.CampaignID); ov != nil {
			overrideApplied = true
			if ov.ViewWindowHours != nil {
				windowHrs = *ov.ViewWindowHours
			}
			if ov.RequireViewable != nil {
				requireViewable = *ov.RequireViewable
			}
		}
	}
	since := base.Add(-time.Duration(windowHrs) * time.Hour)

	imps, err := a.reader.ViewableImpressionsForUsers(ctx, users, e.AccountID, e.CampaignID, since, requireViewable)
	if err != nil {
		a.log.Warn("attribution lookback failed", "conv_trace", e.TraceID, "error", err)
		return false
	}
	a.log.Debug("attribution lookback", "conv_trace", e.TraceID, "campaign", e.CampaignID,
		"window_hrs", windowHrs, "override_applied", overrideApplied,
		"resolved_ids", len(users), "imps", len(imps), "click_through", clickThrough)
	if !clickThrough {
		if len(imps) == 0 {
			return false // no click, no prior exposure → unattributed
		}
		// Last-touch view-through (what settles): reader returns most-recent first.
		e.AttributedTraceID = imps[0].TraceID
		e.AttributionType = "view_through"
		if e.CampaignID == "" {
			e.CampaignID = imps[0].CampaignID
		}
	}
	// Multi-touch (reporting only): record the exposure chain so fractional credit
	// can be computed per model on read. For view-through it's the full chain; for
	// click-through it's the assisting impressions (the click stays last-touch on
	// the conversion). Billing settles last-touch regardless.
	a.recordChain(ctx, e, imps)
	a.log.Debug("conversion attributed", "conv_trace", e.TraceID, "type", e.AttributionType,
		"exposure_trace", e.AttributedTraceID, "chain", len(imps), "resolved_users", len(users))
	return true
}

// recordChain persists the conversion's multi-touch chain (Phase 3). Best-effort
// and reporting-only: a failure here never blocks the last-touch settle.
func (a *viewThroughAttributor) recordChain(ctx context.Context, e *analytics.ConversionEvent, imps []analytics.ViewableImpression) {
	if a.writer == nil || len(imps) == 0 {
		return
	}
	now := time.Now().UTC()
	rows := make([]*analytics.AttributionTouchpointRow, 0, len(imps))
	for _, imp := range imps {
		rows = append(rows, &analytics.AttributionTouchpointRow{
			ConversionTraceID: e.TraceID,
			TouchpointTraceID: imp.TraceID,
			AccountID:         e.AccountID,
			CampaignID:        imp.CampaignID,
			TouchpointType:    "impression",
			TouchpointAt:      imp.Timestamp,
			ConversionAt:      e.Timestamp,
			ConversionRevenue: e.RevenueUSD,
			ObservedAt:        now,
		})
	}
	if err := a.writer.InsertAttributionTouchpoints(ctx, rows); err != nil {
		a.log.Warn("attribution chain write failed", "conv_trace", e.TraceID, "error", err)
	}
}

// resolveUsers expands the advertiser visitor id to the platform user set. Two
// hops: uid → its direct links → their links. That bridges the common shape
// advertiser_uid ↔ shared_id (hashed_email) ↔ publisher_user_id, so an exposure
// recorded under the publisher-side id is reachable. Bounded on purpose. Always
// includes uid itself, so same-id matches work with no graph.
func (a *viewThroughAttributor) resolveUsers(ctx context.Context, uid string) []string {
	set := map[string]struct{}{uid: {}}
	if a.resolver != nil {
		minConf := keys.Attribution.MinIdentityConfidence.Get(a.cfg)
		maxIDs := keys.Attribution.MaxResolvedIDs.Get(a.cfg)
		add := func(ids []string) bool { // returns false once the cap is hit
			for _, id := range ids {
				if len(set) >= maxIDs {
					return false
				}
				set[id] = struct{}{}
			}
			return true
		}
		first, err := a.resolver.ResolveIdentityConfident(ctx, uid, minConf, maxIDs)
		if err != nil {
			a.log.Warn("identity resolve failed", "uid", uid, "error", err)
		}
		ok := add(first)
		for _, id := range first {
			if !ok {
				break
			}
			second, err := a.resolver.ResolveIdentityConfident(ctx, id, minConf, maxIDs)
			if err != nil {
				continue
			}
			ok = add(second)
		}
		if len(set) >= maxIDs {
			a.log.Warn("attribution: resolved-id set hit the cap", "cap", maxIDs, "conv_uid", uid)
		}
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	return out
}
