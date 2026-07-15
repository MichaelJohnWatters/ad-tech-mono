package profilebuilder

import (
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
	MinCount    int    `json:"min_count,omitempty"`   // default 1
	WindowDays  int    `json:"window_days,omitempty"` // default 30
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
	return true
}

// evaluateRule counts matching rows per user key within the window and
// returns the keys meeting min_count. The user key is the row's user_id,
// falling back to household_id for anonymous CTV rows — mirroring the
// serve-path lookup precedence.
func evaluateRule(rows []datalake.Record, rule Rule, now time.Time) []string {
	cutoff := now.AddDate(0, 0, -rule.WindowDays)
	counts := map[string]int{}
	for _, rec := range rows {
		if !rule.matches(rec) {
			continue
		}
		if at, ok := rec["observed_at"].(time.Time); ok && at.Before(cutoff) {
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
	}
	var out []string
	for key, n := range counts {
		if n >= rule.MinCount {
			out = append(out, key)
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
