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
	ID        string
	AccountID string
	Type      string // always "retargeting" for the sources we query
	Rule      []byte // JSONB rule; see rule below
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
	AddMembers(ctx context.Context, accountID, segmentID string, userIDs []string, ttl time.Duration) (int, error)
	RemoveMember(ctx context.Context, accountID, segmentID, userID string) (int, error)
	InvalidateAudience(ctx context.Context) error
}

// Service applies visit → enroll and conversion → suppress against a segment
// source and enroller.
type Service struct {
	src SegmentSource
	enr Enroller
	log *slog.Logger
}

func New(src SegmentSource, enr Enroller, log *slog.Logger) *Service {
	return &Service{src: src, enr: enr, log: log}
}

const kindSiteVisit = "site_visit"

// OnSiteVisit enrolls the visitor into every retargeting segment of the
// advertiser (ev.AccountID) whose rule fires on a single site visit: event =
// site_visit, min_count <= 1, and tag matching (empty tag matches any). Returns
// the segment ids newly enrolled. A no-op for non-visit events or missing ids.
func (s *Service) OnSiteVisit(ctx context.Context, ev events.BehaviourSignalEvent) ([]string, error) {
	if ev.Kind != kindSiteVisit || ev.UserID == "" || ev.AccountID == "" {
		return nil, nil
	}
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
		n, err := s.enr.AddMembers(ctx, ev.AccountID, seg.ID, []string{ev.UserID}, ttl)
		if err != nil {
			s.log.Warn("retargeting enroll failed", "segment", seg.ID, "account", ev.AccountID, "error", err)
			continue
		}
		if n > 0 {
			enrolled = append(enrolled, seg.ID)
		}
	}
	if len(enrolled) > 0 {
		if err := s.enr.InvalidateAudience(ctx); err != nil {
			s.log.Warn("audience invalidate after enroll failed", "error", err)
		}
		s.log.Info("real-time retargeting enroll", "account", ev.AccountID, "user", ev.UserID, "segments", len(enrolled))
	}
	return enrolled, nil
}

// OnConversion suppresses a converter: it removes the user from the advertiser's
// retargeting segments so we stop paying to chase someone who already bought.
// Returns the segment ids the user was removed from.
func (s *Service) OnConversion(ctx context.Context, accountID, userID string) ([]string, error) {
	if accountID == "" || userID == "" {
		return nil, nil
	}
	segs, err := s.src.RetargetingSegments(ctx, accountID)
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, seg := range segs {
		n, err := s.enr.RemoveMember(ctx, accountID, seg.ID, userID)
		if err != nil {
			s.log.Warn("retargeting suppress failed", "segment", seg.ID, "account", accountID, "error", err)
			continue
		}
		if n > 0 {
			removed = append(removed, seg.ID)
		}
	}
	if len(removed) > 0 {
		if err := s.enr.InvalidateAudience(ctx); err != nil {
			s.log.Warn("audience invalidate after suppress failed", "error", err)
		}
		s.log.Info("real-time retargeting suppress", "account", accountID, "user", userID, "segments", len(removed))
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
