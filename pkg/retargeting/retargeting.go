// Package retargeting turns a shopper's site visit into audience membership in
// REAL TIME, instead of waiting for the hourly batch profile-builder. The
// retargeting pixel already publishes a `site_visit` behaviour signal; a consumer
// feeds those (and purchase conversions) here, and this package enrolls the
// visitor into the advertiser's retargeting segment(s) immediately and suppresses
// them the moment they convert.
//
// It deliberately reuses the existing audience path end to end: it writes the same
// audience_segment_members rows the batch builder would, so the DSP retargets on
// them with no new bid-time code — the only change is latency (seconds, not up to
// an hour). Rules that need visit-frequency history (min_count > 1) stay with the
// batch builder; a single-visit rule (min_count <= 1) is what fires instantly.
package retargeting

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
)

// Segment is an advertiser's audience segment plus its raw rule JSON
// (audience_segments.rule).
type Segment struct {
	ID         string
	AccountID  string
	Type       string // always "retargeting" for the sources we query
	Rule       []byte // JSONB rule; see rule below
	Visibility string // public | dsp_private — for the cache change-log's Redis-set key
}

// rule is the subset of a segment's rule this package evaluates for real-time
// enrollment. It mirrors pkg/profilebuilder.Rule's JSON so the two agree on what
// a site_visit segment means.
type rule struct {
	Event      string `json:"event"`
	Tag        string `json:"tag,omitempty"`
	MinCount   int    `json:"min_count,omitempty"`
	WindowDays int    `json:"window_days,omitempty"`
}

// defaultWindowDays matches pkg/profilebuilder.Rule's default window: a
// retargeting member ages out this many days after the visit unless the rule
// sets its own window_days.
const defaultWindowDays = 30

// SegmentSource lists an advertiser's active retargeting segments.
type SegmentSource interface {
	RetargetingSegments(ctx context.Context, accountID string) ([]Segment, error)
}

// Enroller writes/removes segment membership and invalidates the audience cache
// so the DSP's preloader refreshes and the change is live within seconds.
type Enroller interface {
	// AddMembers enrolls users into a segment with a time-to-live: after ttl the
	// member ages out (read paths exclude expired rows). ttl<=0 means no expiry.
	// visibility is the segment's (public|dsp_private) — the cache change-log needs
	// it so the delta lands in the right Redis set. originTrace is the enrolling
	// site-visit's trace_id, stamped as the membership row's lineage (migration
	// 080) so "why is this user in this segment" survives past the transient
	// retargeting.enrolled event.
	AddMembers(ctx context.Context, accountID, segmentID string, userIDs []string, ttl time.Duration, visibility, originTrace string) (int, error)
	RemoveMember(ctx context.Context, accountID, segmentID, userID, visibility string) (int, error)
	// InvalidateAudience announces a membership change for one user so the DSP/SSP
	// preloader re-materializes just that user's Redis keys (delta refresh), rather
	// than rescanning the whole table. userID is the enrolled/suppressed visitor.
	InvalidateAudience(ctx context.Context, userID string) error
}

// SuppressionStore persists the purchase "burn list" (migration 082): one
// row per (advertiser, user id) recording WHEN the purchase happened.
// Enrollment paths consult it with newer-than semantics — a signal older
// than suppressed_at never re-enrolls; a genuinely new visit clears the row.
type SuppressionStore interface {
	Suppress(ctx context.Context, accountID string, userIDs []string, at time.Time, ttl time.Duration, originTrace string) error
	SuppressedAt(ctx context.Context, accountID, userID string) (time.Time, bool, error)
	ClearSuppression(ctx context.Context, accountID, userID string) error
	// SuppressSKUs writes per-product burn rows (DPA slice 4) so a bought SKU
	// isn't re-featured by a stale pixel.
	SuppressSKUs(ctx context.Context, accountID string, userIDs, skus []string, at time.Time, ttl time.Duration, originTrace string) error
}

// IdentityExpander returns the additional ids a converting user's purchase
// should suppress: identity-cluster siblings (the person's other devices)
// plus linked household ids — matching the person-level expansion the batch
// profile-builder applies when ENROLLING, so suppression and enrollment
// operate at the same granularity.
type IdentityExpander interface {
	ExpandPerson(ctx context.Context, userID string) ([]string, error)
}

// ProductViewStore records the SKUs a shopper viewed/carted (Dynamic Product
// Ads slice 2). Written on a site_visit that carries SKUs; read at render time
// (slice 3) and for per-product suppression (slice 4). Optional — nil keeps
// the pre-DPA behaviour (SKUs ignored).
type ProductViewStore interface {
	RecordProductViews(ctx context.Context, accountID, userID string, skus []string, at time.Time, ttl time.Duration, originTrace string) error
	RemoveProductViews(ctx context.Context, accountID, userID string, skus []string) error
	// CountProductViews reports how many carted SKUs the user still has — used
	// to decide whether a SKU purchase cleared the cart (DPA slice 4).
	CountProductViews(ctx context.Context, accountID, userID string) (int, error)
}

// ComplementSource returns the cross-sell complement SKUs for a set of bought
// SKUs (DPA slice 4). Optional — nil means "no cross-sell" (a purchase just
// suppresses the bought products).
type ComplementSource interface {
	ComplementSKUs(ctx context.Context, accountID string, skus []string) ([]string, error)
}

// Service applies visit → enroll and conversion → suppress against a segment
// source and enroller.
type Service struct {
	src SegmentSource
	enr Enroller
	log *slog.Logger
	// householdEnroll gates enrolling the site_visit's HOUSEHOLD id (hh:
	// salted-IP hash, derived by the tracker's rt pixel) alongside the
	// visitor id — the anonymous-guest-cart chase: no email bridge, the
	// chase reaches any device in the home via the DSP's household-keyed
	// segment lookup. nil/false = visitor-id-only (the original behaviour).
	householdEnroll func() bool
	// sup + expand + suppressionTTL wire the durable purchase burn-list
	// (all optional: nil keeps the pre-082 delete-only behaviour).
	sup            SuppressionStore
	expand         IdentityExpander
	suppressionTTL func() time.Duration
	// products + productViewTTL wire SKU-aware retargeting memory (DPA slice 2).
	// nil products = SKUs on a pixel are ignored (pre-DPA behaviour).
	products       ProductViewStore
	productViewTTL func() time.Duration
	// complements wires cross-sell (DPA slice 4); nil = suppress-only on purchase.
	complements ComplementSource
}

func New(src SegmentSource, enr Enroller, log *slog.Logger) *Service {
	return &Service{src: src, enr: enr, log: log}
}

// SetSuppression wires the burn-list store, the person/household expander,
// and the suppression TTL (bounds row lifetime; past every rule window the
// historical signals can't qualify anyone, so the row is inert).
func (s *Service) SetSuppression(store SuppressionStore, expand IdentityExpander, ttl func() time.Duration) {
	s.sup, s.expand, s.suppressionTTL = store, expand, ttl
}

// SetHouseholdEnroll wires the live-config gate for household enrollment
// (audience_rt.household_enroll). Read per event so a config flip applies
// without a restart.
func (s *Service) SetHouseholdEnroll(fn func() bool) { s.householdEnroll = fn }

// SetProductViews wires SKU-aware retargeting memory (DPA slice 2): the store
// that records a shopper's viewed/carted SKUs and the TTL bounding their
// lifetime (defaults to the retargeting window).
func (s *Service) SetProductViews(store ProductViewStore, ttl func() time.Duration) {
	s.products, s.productViewTTL = store, ttl
}

// SetComplements wires cross-sell (DPA slice 4): after a SKU purchase the chase
// rotates toward these complementary catalog items. nil = suppress-only.
func (s *Service) SetComplements(src ComplementSource) { s.complements = src }

const kindSiteVisit = "site_visit"

// OnSiteVisit enrolls the visitor into every retargeting segment of the
// advertiser (ev.AccountID) whose rule fires on a single site visit: event =
// site_visit, min_count <= 1, and tag matching (empty tag matches any). Returns
// the segment ids newly enrolled. A no-op for non-visit events or missing ids.
func (s *Service) OnSiteVisit(ctx context.Context, ev events.BehaviourSignalEvent) ([]string, error) {
	if ev.Kind != kindSiteVisit || ev.UserID == "" || ev.AccountID == "" {
		return nil, nil
	}
	// DPA slice 2: remember the shopper's viewed/carted SKUs (independent of
	// segment matching — a product-page pixel builds the memory a later
	// checkout's dynamic creative renders from). Person+household, like enroll.
	s.recordProductViews(ctx, ev)
	segs, err := s.src.RetargetingSegments(ctx, ev.AccountID)
	if err != nil {
		return nil, err
	}
	var enrolled []string
	for _, seg := range segs {
		r, ok := parseRule(seg.Rule)
		if !ok || r.Event != kindSiteVisit {
			continue
		}
		if r.MinCount > 1 {
			// Needs visit-frequency history — the batch profile-builder owns it.
			continue
		}
		if r.Tag != "" && !strings.EqualFold(strings.TrimSpace(r.Tag), strings.TrimSpace(ev.Tag)) {
			continue
		}
		// TTL so an abandoner who never converts ages out of the audience after
		// the rule's window (default 30 days) instead of being chased forever.
		windowDays := r.WindowDays
		if windowDays <= 0 {
			windowDays = defaultWindowDays
		}
		ttl := time.Duration(windowDays) * 24 * time.Hour
		members := []string{ev.UserID}
		if s.householdEnroll != nil && s.householdEnroll() && ev.HouseholdID != "" && ev.HouseholdID != ev.UserID {
			members = append(members, ev.HouseholdID)
		}
		members = s.applySuppression(ctx, ev, members)
		if len(members) == 0 {
			continue
		}
		n, err := s.enr.AddMembers(ctx, ev.AccountID, seg.ID, members, ttl, seg.Visibility, ev.TraceID)
		if err != nil {
			s.log.Warn("retargeting enroll failed", "segment", seg.ID, "account", ev.AccountID, "trace_id", ev.TraceID, "error", err)
			continue
		}
		if n > 0 {
			enrolled = append(enrolled, seg.ID)
		}
	}
	if len(enrolled) > 0 {
		if err := s.enr.InvalidateAudience(ctx, ev.UserID); err != nil {
			s.log.Warn("audience invalidate after enroll failed", "trace_id", ev.TraceID, "error", err)
		}
		s.log.Info("real-time retargeting enroll", "account", ev.AccountID, "user", ev.UserID, "segments", len(enrolled), "trace_id", ev.TraceID)
	}
	return enrolled, nil
}

// recordProductViews stores the SKUs an ev carried for the visitor (and the
// household id when household enrollment is on), with the product-view TTL.
// A no-op when no store is wired or the pixel carried no SKUs.
func (s *Service) recordProductViews(ctx context.Context, ev events.BehaviourSignalEvent) {
	if s.products == nil || ev.SKUs == "" {
		return
	}
	skus := splitSKUs(ev.SKUs)
	if len(skus) == 0 {
		return
	}
	ttl := time.Duration(defaultWindowDays) * 24 * time.Hour
	if s.productViewTTL != nil {
		if v := s.productViewTTL(); v > 0 {
			ttl = v
		}
	}
	at := ev.ObservedAt
	if at.IsZero() {
		at = time.Now().UTC()
	}
	ids := []string{ev.UserID}
	if s.householdEnroll != nil && s.householdEnroll() && ev.HouseholdID != "" && ev.HouseholdID != ev.UserID {
		ids = append(ids, ev.HouseholdID)
	}
	for _, id := range ids {
		if err := s.products.RecordProductViews(ctx, ev.AccountID, id, skus, at, ttl, ev.TraceID); err != nil {
			s.log.Warn("record product views failed", "account", ev.AccountID, "user", id, "trace_id", ev.TraceID, "error", err)
		}
	}
}

// splitSKUs parses a comma-separated SKU list into trimmed, non-empty,
// de-duplicated entries (order preserved), capped to bound a pathological pixel.
func splitSKUs(csv string) []string {
	parts := strings.Split(csv, ",")
	out := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
		if len(out) >= 20 {
			break
		}
	}
	return out
}

// OnConversion suppresses a converter: it removes the user from the advertiser's
// retargeting segments so we stop paying to chase someone who already bought.
// Returns the segment ids the user was removed from.
// applySuppression drops candidate ids whose burn-list entry is at/after the
// visit — the purchase closed that cart, old signals must not re-enroll. A
// visit NEWER than the suppression is a genuinely new abandoned cart: the
// row is cleared (self-healing) and the id enrolls normally. No suppression
// store wired = everything passes (pre-082 behaviour).
func (s *Service) applySuppression(ctx context.Context, ev events.BehaviourSignalEvent, ids []string) []string {
	if s.sup == nil {
		return ids
	}
	at := ev.ObservedAt
	if at.IsZero() {
		at = time.Now().UTC()
	}
	kept := ids[:0]
	for _, id := range ids {
		supAt, ok, err := s.sup.SuppressedAt(ctx, ev.AccountID, id)
		if err != nil {
			s.log.Warn("suppression lookup failed — enrolling anyway", "user", id, "account", ev.AccountID, "error", err)
			kept = append(kept, id)
			continue
		}
		if !ok || at.After(supAt) {
			if ok { // new cart after the purchase — burn-list entry served its purpose
				if err := s.sup.ClearSuppression(ctx, ev.AccountID, id); err != nil {
					s.log.Warn("suppression clear failed", "user", id, "account", ev.AccountID, "error", err)
				}
			}
			kept = append(kept, id)
		}
	}
	return kept
}

// suppressionIDs is the person+household expansion of a converting user: the
// id itself, its identity-cluster siblings, and linked household ids. Capped
// to bound pathological clusters.
func (s *Service) suppressionIDs(ctx context.Context, userID string) []string {
	ids := []string{userID}
	if s.expand == nil {
		return ids
	}
	more, err := s.expand.ExpandPerson(ctx, userID)
	if err != nil {
		s.log.Warn("suppression identity expansion failed — suppressing the converting id only", "user", userID, "error", err)
		return ids
	}
	seen := map[string]bool{userID: true}
	for _, id := range more {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
		if len(ids) >= 25 { // pathological-cluster guard
			break
		}
	}
	return ids
}

// OnConversion handles a purchase. A GENERIC conversion (no bought SKUs) does
// whole-person suppression as before: remove the buyer (+ cluster/household)
// from every retargeting segment and burn-list them. A SKU-carrying purchase
// (Dynamic Product Ads slice 4) is PER-PRODUCT: suppress the bought SKUs, rotate
// the dynamic creative to their cross-sell complements, and keep chasing the
// rest of the cart — only falling through to whole-person suppression when
// nothing remains to show. Returns the segment ids removed (nil on the
// keep-chasing per-product path).
func (s *Service) OnConversion(ctx context.Context, accountID, userID, traceID string, boughtSKUs []string) ([]string, error) {
	if accountID == "" || userID == "" {
		return nil, nil
	}
	ids := s.suppressionIDs(ctx, userID)
	if len(boughtSKUs) > 0 && s.products != nil {
		return s.onProductConversion(ctx, accountID, userID, traceID, ids, boughtSKUs)
	}
	return s.onGenericConversion(ctx, accountID, userID, traceID, ids)
}

// onProductConversion is the per-product purchase path (DPA slice 4): burn +
// remove the bought SKUs, cross-sell their complements, then keep chasing if
// anything remains, else fall through to whole-person suppression.
func (s *Service) onProductConversion(ctx context.Context, accountID, userID, traceID string, ids, boughtSKUs []string) ([]string, error) {
	now := time.Now().UTC()
	burnTTL := 30 * 24 * time.Hour
	if s.suppressionTTL != nil {
		if v := s.suppressionTTL(); v > 0 {
			burnTTL = v
		}
	}
	viewTTL := time.Duration(defaultWindowDays) * 24 * time.Hour
	if s.productViewTTL != nil {
		if v := s.productViewTTL(); v > 0 {
			viewTTL = v
		}
	}
	// Durable per-SKU burn for every id (stops a stale pixel re-adding a bought
	// product), then remove the bought SKUs from the carted-products memory.
	if s.sup != nil {
		if err := s.sup.SuppressSKUs(ctx, accountID, ids, boughtSKUs, now, burnTTL, traceID); err != nil {
			s.log.Warn("per-product burn write failed", "account", accountID, "trace_id", traceID, "error", err)
		}
	}
	for _, id := range ids {
		if err := s.products.RemoveProductViews(ctx, accountID, id, boughtSKUs); err != nil {
			s.log.Warn("per-product view removal failed", "user", id, "account", accountID, "error", err)
		}
	}
	// Cross-sell: rotate the chase toward the bought products' complements
	// (RecordProductViews skips any that are themselves burn-listed).
	var complements []string
	if s.complements != nil {
		if c, err := s.complements.ComplementSKUs(ctx, accountID, boughtSKUs); err != nil {
			s.log.Warn("cross-sell complement lookup failed", "account", accountID, "error", err)
		} else if len(c) > 0 {
			complements = c
			for _, id := range ids {
				if err := s.products.RecordProductViews(ctx, accountID, id, complements, now, viewTTL, traceID); err != nil {
					s.log.Warn("cross-sell record failed", "user", id, "account", accountID, "error", err)
				}
			}
		}
	}
	// Cart-clear check on the converting id: nothing left to feature (bought
	// everything, no complement) → whole-person suppress. Otherwise keep chasing
	// the remaining cart + cross-sell.
	remaining, err := s.products.CountProductViews(ctx, accountID, userID)
	if err != nil {
		s.log.Warn("product-view count failed — treating as cart-cleared", "user", userID, "account", accountID, "error", err)
		remaining = 0
	}
	if remaining == 0 {
		return s.onGenericConversion(ctx, accountID, userID, traceID, ids)
	}
	for _, id := range ids {
		if err := s.enr.InvalidateAudience(ctx, id); err != nil {
			s.log.Warn("audience invalidate after per-product suppress failed", "trace_id", traceID, "error", err)
		}
	}
	s.log.Info("real-time per-product suppress", "account", accountID, "user", userID,
		"bought", len(boughtSKUs), "complements", len(complements), "remaining", remaining, "trace_id", traceID)
	return nil, nil
}

// onGenericConversion is the whole-person suppression path (a purchase with no
// SKU context, or a SKU purchase that cleared the cart): remove the buyer from
// every retargeting segment and burn-list the person + household.
func (s *Service) onGenericConversion(ctx context.Context, accountID, userID, traceID string, ids []string) ([]string, error) {
	segs, err := s.src.RetargetingSegments(ctx, accountID)
	if err != nil {
		return nil, err
	}
	// Person+household suppression: remove EVERY id the buyer expands to
	// (cluster siblings + household), not just the id on the conversion —
	// otherwise the batch builder's person-level enrollment keeps the
	// buyer's other devices in the chase.
	var removed []string
	for _, seg := range segs {
		got := 0
		for _, id := range ids {
			n, err := s.enr.RemoveMember(ctx, accountID, seg.ID, id, seg.Visibility)
			if err != nil {
				s.log.Warn("retargeting suppress failed", "segment", seg.ID, "account", accountID, "user", id, "trace_id", traceID, "error", err)
				continue
			}
			got += n
		}
		if got > 0 {
			removed = append(removed, seg.ID)
		}
	}
	// Durable burn-list entries for ALL ids — the profile-builder consults
	// these so its next pass cannot re-qualify the buyer from pre-purchase
	// signals (the "re-enrolled at :10" gap, found live 2026-08-09).
	if s.sup != nil {
		ttl := 30 * 24 * time.Hour
		if s.suppressionTTL != nil {
			if v := s.suppressionTTL(); v > 0 {
				ttl = v
			}
		}
		if err := s.sup.Suppress(ctx, accountID, ids, time.Now().UTC(), ttl, traceID); err != nil {
			s.log.Warn("burn-list write failed — suppression is delete-only for this purchase", "account", accountID, "trace_id", traceID, "error", err)
		}
	}
	if len(removed) > 0 || s.sup != nil {
		for _, id := range ids {
			if err := s.enr.InvalidateAudience(ctx, id); err != nil {
				s.log.Warn("audience invalidate after suppress failed", "trace_id", traceID, "error", err)
			}
		}
		s.log.Info("real-time retargeting suppress", "account", accountID, "user", userID, "expanded_ids", len(ids), "segments", len(removed), "trace_id", traceID)
	}
	return removed, nil
}

func parseRule(raw []byte) (rule, bool) {
	if len(raw) == 0 {
		return rule{}, false
	}
	var r rule
	if err := json.Unmarshal(raw, &r); err != nil || r.Event == "" {
		return rule{}, false
	}
	return r, true
}
