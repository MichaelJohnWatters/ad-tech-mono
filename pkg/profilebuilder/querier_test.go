package profilebuilder

import (
	"strings"
	"testing"
	"time"
)

// The SQL builders are the parity contract with evaluateRule: a subtle drift
// changes who's in an audience. These assert the generated SQL encodes the
// rule's event/tag/category/window/min_count/account semantics and binds every
// user-supplied value (no injection).

func TestQualifyingUsersSQL_Behavioural(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	rule := Rule{Event: "click", MinCount: 3, WindowDays: 14}
	q, args := qualifyingUsersSQL(rule, now)

	if !strings.Contains(q, "kind = ?") {
		t.Errorf("event not filtered by kind: %s", q)
	}
	if !strings.Contains(q, "observed_at >= ?") {
		t.Errorf("window not applied as >= cutoff: %s", q)
	}
	if !strings.Contains(q, "count() >= ?") {
		t.Errorf("min_count not applied in HAVING: %s", q)
	}
	if !strings.Contains(q, "if(user_id != '', user_id, household_id)") {
		t.Errorf("user_id→household_id key precedence missing: %s", q)
	}
	if !strings.Contains(q, "GROUP BY key") {
		t.Errorf("per-user grouping missing: %s", q)
	}
	// args: kind, cutoff, min_count (no optional filters set)
	if len(args) != 3 {
		t.Fatalf("args = %v, want [kind cutoff min_count]", args)
	}
	if args[0] != "click" {
		t.Errorf("first arg = %v, want kind 'click'", args[0])
	}
	wantCutoff := now.AddDate(0, 0, -14)
	if got, ok := args[1].(time.Time); !ok || !got.Equal(wantCutoff) {
		t.Errorf("cutoff arg = %v, want %v (now - window_days)", args[1], wantCutoff)
	}
	if args[2] != uint64(3) {
		t.Errorf("min_count arg = %v, want uint64(3)", args[2])
	}
}

func TestQualifyingUsersSQL_AllFiltersBound(t *testing.T) {
	now := time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)
	rule := Rule{
		Event: "site_visit", Tag: "product; DROP TABLE x", Category: "Autos & Vehicles",
		Channel: "web", CampaignID: "cmp1", PublisherID: "pub1",
		MinCount: 1, WindowDays: 30, accountID: "acct-1",
	}
	q, args := qualifyingUsersSQL(rule, now)

	for _, frag := range []string{
		"kind = ?", "channel = ?", "campaign_id = ?", "publisher_id = ?",
		"tag = ?", "account_id = ?",
	} {
		if !strings.Contains(q, frag) {
			t.Errorf("missing bound filter %q in: %s", frag, q)
		}
	}
	// Category uses case-insensitive membership over the split categories.
	if !strings.Contains(q, "splitByChar(',', categories)") || !strings.Contains(q, "has(") {
		t.Errorf("category not matched via hasCategory-equivalent membership: %s", q)
	}
	// No raw value is interpolated — the injection attempt rides as a param.
	if strings.Contains(q, "DROP TABLE") {
		t.Fatalf("tag value interpolated into SQL (injection): %s", q)
	}
	foundTag, foundAccount, foundCategory := false, false, false
	for _, a := range args {
		switch a {
		case "product; DROP TABLE x":
			foundTag = true
		case "acct-1":
			foundAccount = true
		case "Autos & Vehicles":
			foundCategory = true
		}
	}
	if !foundTag || !foundAccount || !foundCategory {
		t.Errorf("tag/account/category not passed as bound args: %v", args)
	}
}

func TestQualifyingUsersSQL_NoAccountScopeWhenUnset(t *testing.T) {
	now := time.Now().UTC()
	// A non-site_visit rule leaves accountID empty → no account_id filter.
	q, _ := qualifyingUsersSQL(Rule{Event: "impression", MinCount: 1, WindowDays: 30}, now)
	if strings.Contains(q, "account_id = ?") {
		t.Errorf("account scope applied when accountID unset: %s", q)
	}
}

func TestCategorySignalsSQL(t *testing.T) {
	now := time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)
	q, args := categorySignalsSQL(31, now)
	if !strings.Contains(q, "SELECT DISTINCT key, category") {
		t.Errorf("not distinct (key, category): %s", q)
	}
	if !strings.Contains(q, "arrayJoin(splitByChar(',', categories))") {
		t.Errorf("categories not split/exploded: %s", q)
	}
	if !strings.Contains(q, "lower(trim(BOTH ' ' FROM") {
		t.Errorf("categories not trimmed + lower-cased (personCategories parity): %s", q)
	}
	if !strings.Contains(q, "if(user_id != '', user_id, household_id)") {
		t.Errorf("key precedence missing: %s", q)
	}
	if len(args) != 1 {
		t.Fatalf("args = %v, want [cutoff]", args)
	}
	wantCutoff := now.AddDate(0, 0, -31)
	if got, ok := args[0].(time.Time); !ok || !got.Equal(wantCutoff) {
		t.Errorf("cutoff = %v, want %v", args[0], wantCutoff)
	}
}

func TestSegmentMembershipsSQL(t *testing.T) {
	q := segmentMembershipsSQL()
	if !strings.Contains(q, "groupUniqArray(id_value)") {
		t.Errorf("id_values not deduped server-side: %s", q)
	}
	if !strings.Contains(q, "GROUP BY account_id, segment_id") {
		t.Errorf("not grouped by (account, segment): %s", q)
	}
	for _, frag := range []string{"account_id != ''", "segment_id != ''", "id_value != ''"} {
		if !strings.Contains(q, frag) {
			t.Errorf("missing empty-skip %q (reconcileProfileSignals parity): %s", frag, q)
		}
	}
}
