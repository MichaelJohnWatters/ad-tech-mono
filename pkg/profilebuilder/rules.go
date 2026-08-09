package profilebuilder

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/datalake"
)

// Rule is the behavioural segmentation rule stored as JSONB on
// audience_segments.rule: "enroll every person with ≥ min_count matching
// behaviour_signals rows in the last window_days". Empty filter fields match
// everything.
//
// Evaluated in pure Go over the Delta reader rather than DuckDB: the lake
// reader (pkg/store/datalake) is CGO-free, while DuckDB would drag the
// zig-cross-compile build machinery (see the reporting hot/cold store) into
// a CronJob binary. The rule shape is engine-agnostic JSONB, so a DuckDB
// evaluator can swap in behind the same rows when scale demands it.
type Rule struct {
	Event       string `json:"event"` // request | impression | click | conversion | view
	Category    string `json:"category,omitempty"`
	Channel     string `json:"channel,omitempty"`
	CampaignID  string `json:"campaign_id,omitempty"`
	PublisherID string `json:"publisher_id,omitempty"`
	// Tag filters retargeting-pixel rows (event "site_visit") by the
	// advertiser's self-chosen pixel tag.
	Tag        string `json:"tag,omitempty"`
	MinCount   int    `json:"min_count,omitempty"`   // default 1
	WindowDays int    `json:"window_days,omitempty"` // default 30

	// accountID is set by the builder, never from JSONB: site_visit rows
	// are scoped to the segment's own account so pixel tags can't collide
	// across tenants.
	accountID string
}

// RuleKind extracts the rule's kind for dispatch: "" or "behaviour" →
// behavioural (the original rule shape); "composite" and "lookalike" are
// the derived-segment kinds evaluated AFTER behavioural rules in a run.
func RuleKind(raw []byte) string {
	var probe struct {
		Kind string `json:"kind"`
	}
	_ = json.Unmarshal(raw, &probe)
	return probe.Kind
}

// CompositeRule derives a segment from OTHER segments with boolean set
// logic, evaluated at PERSON level: a person qualifies when it has a member
// in every all_of segment, at least one any_of segment (when set), and no
// none_of segment. Referenced segments must belong to the same account.
// Composite-of-composite sees the PREVIOUS run's members (single pass).
type CompositeRule struct {
	AllOf  []string `json:"all_of,omitempty"`
	AnyOf  []string `json:"any_of,omitempty"`
	NoneOf []string `json:"none_of,omitempty"`
}

// ParseCompositeRule decodes + validates a composite rule.
func ParseCompositeRule(raw []byte) (CompositeRule, error) {
	var r CompositeRule
	if err := json.Unmarshal(raw, &r); err != nil {
		return r, fmt.Errorf("parse composite rule: %w", err)
	}
	if len(r.AllOf)+len(r.AnyOf) == 0 {
		return r, fmt.Errorf("composite rule needs all_of or any_of")
	}
	return r, nil
}

// LookalikeRule derives a segment of persons whose behavioural category
// profile resembles a seed segment's. Heuristic MVP (deliberately simple +
// deterministic, the rule shape leaves room for a model later): take the
// seed persons' TOP-K categories, score every other person by the fraction
// of those categories they share, enroll score ≥ min_similarity up to
// max_members.
type LookalikeRule struct {
	SeedSegment   string  `json:"seed_segment"`
	TopCategories int     `json:"top_categories,omitempty"` // default 10
	MinSimilarity float64 `json:"min_similarity,omitempty"` // default 0.5
	MaxMembers    int     `json:"max_members,omitempty"`    // default 1000
}

// ParseLookalikeRule decodes + defaults a lookalike rule.
func ParseLookalikeRule(raw []byte) (LookalikeRule, error) {
	var r LookalikeRule
	if err := json.Unmarshal(raw, &r); err != nil {
		return r, fmt.Errorf("parse lookalike rule: %w", err)
	}
	if r.SeedSegment == "" {
		return r, fmt.Errorf("lookalike rule needs seed_segment")
	}
	if r.TopCategories < 1 {
		r.TopCategories = 10
	}
	if r.MinSimilarity <= 0 {
		r.MinSimilarity = 0.5
	}
	if r.MaxMembers < 1 {
		r.MaxMembers = 1000
	}
	return r, nil
}

// ParseRule decodes + defaults a segment's rule JSONB.
func ParseRule(raw []byte) (Rule, error) {
	var r Rule
	if err := json.Unmarshal(raw, &r); err != nil {
		return r, fmt.Errorf("parse rule: %w", err)
	}
	if r.Event == "" {
		return r, fmt.Errorf("rule missing event")
	}
	if r.MinCount < 1 {
		r.MinCount = 1
	}
	if r.WindowDays < 1 {
		r.WindowDays = 30
	}
	return r, nil
}

// matches reports whether one behaviour_signals row satisfies the rule's
// filters (the count/window logic lives in evaluateRule).
func (r Rule) matches(rec datalake.Record) bool {
	if str(rec["kind"]) != r.Event {
		return false
	}
	if r.Channel != "" && str(rec["channel"]) != r.Channel {
		return false
	}
	if r.CampaignID != "" && str(rec["campaign_id"]) != r.CampaignID {
		return false
	}
	if r.PublisherID != "" && str(rec["publisher_id"]) != r.PublisherID {
		return false
	}
	if r.Category != "" && !hasCategory(str(rec["categories"]), r.Category) {
		return false
	}
	if r.Tag != "" && str(rec["tag"]) != r.Tag {
		return false
	}
	if r.accountID != "" && str(rec["account_id"]) != r.accountID {
		return false
	}
	return true
}

// MaxRuleWindowDays returns the largest window_days any active rule-driven
// segment declares (floor: the 30-day default), across ALL accounts — the
// builder is the cross-tenant expansion engine, and this bounds how far back
// its behaviour_signals lake read must reach. Daily partition pruning then
// skips every older file, so keep-forever cold storage costs the builder
// nothing. +1 day of slack absorbs the partition-granularity edge (a row
// late on the cutoff day lives in a partition that starts before it).
func MaxRuleWindowDays(ctx context.Context, db *sql.DB) (int, error) {
	var maxDays int
	err := platformReadTx(ctx, db, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
SELECT COALESCE(MAX(GREATEST(COALESCE((rule->>'window_days')::int, 30), 30)), 30)
FROM audience_segments
WHERE rule IS NOT NULL AND status = 'active'`).Scan(&maxDays)
	})
	if err != nil {
		return 0, err
	}
	return maxDays + 1, nil
}

// evaluateRule counts matching rows per user key within the window and
// returns the keys meeting min_count. The user key is the row's user_id,
// falling back to household_id for anonymous CTV rows — mirroring the
// serve-path lookup precedence.
func evaluateRule(rows []datalake.Record, rule Rule, now time.Time) []QualifiedKey {
	cutoff := now.AddDate(0, 0, -rule.WindowDays)
	counts := map[string]int{}
	last := map[string]time.Time{}
	for _, rec := range rows {
		if !rule.matches(rec) {
			continue
		}
		at, hasAt := rec["observed_at"].(time.Time)
		if hasAt && at.Before(cutoff) {
			continue
		}
		key := str(rec["user_id"])
		if key == "" {
			key = str(rec["household_id"])
		}
		if key == "" {
			continue
		}
		counts[key]++
		if hasAt && at.After(last[key]) {
			last[key] = at
		}
	}
	var out []QualifiedKey
	for key, n := range counts {
		if n >= rule.MinCount {
			out = append(out, QualifiedKey{Key: key, Last: last[key]})
		}
	}
	return out
}

func hasCategory(csv, want string) bool {
	for _, c := range strings.Split(csv, ",") {
		if strings.EqualFold(strings.TrimSpace(c), want) {
			return true
		}
	}
	return false
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
