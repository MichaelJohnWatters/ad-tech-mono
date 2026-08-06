package targeting_test

import (
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/targeting"
)

func TestEvaluate_MatchAll(t *testing.T) {
	rules := targeting.Rules{} // empty = match everything
	req := targeting.Request{Geo: "UK", Device: "mobile"}

	result := targeting.Evaluate(rules, req)
	if !result.Matched {
		t.Error("empty rules should match everything")
	}
}

func TestEvaluate_GeoInclusion(t *testing.T) {
	rules := targeting.Rules{
		Include: targeting.TargetingSet{Geo: []string{"UK", "DE", "FR"}},
	}

	// UK should match
	result := targeting.Evaluate(rules, targeting.Request{Geo: "UK"})
	if !result.Matched {
		t.Error("UK should match geo inclusion [UK, DE, FR]")
	}

	// US should NOT match
	result = targeting.Evaluate(rules, targeting.Request{Geo: "US"})
	if result.Matched {
		t.Error("US should NOT match geo inclusion [UK, DE, FR]")
	}
}

func TestEvaluate_GeoHierarchy(t *testing.T) {
	rules := targeting.Rules{
		Include: targeting.TargetingSet{Geo: []string{"UK"}},
	}

	// UK_london should match (sub-region of UK)
	result := targeting.Evaluate(rules, targeting.Request{Geo: "UK_london"})
	if !result.Matched {
		t.Error("UK_london should match geo inclusion [UK] via hierarchy")
	}
}

func TestEvaluate_MultiDimensionAND(t *testing.T) {
	rules := targeting.Rules{
		Include: targeting.TargetingSet{
			Geo:    []string{"UK"},
			Device: []string{"mobile"},
		},
	}

	// UK + mobile = match (both dimensions)
	result := targeting.Evaluate(rules, targeting.Request{Geo: "UK", Device: "mobile"})
	if !result.Matched {
		t.Error("UK + mobile should match")
	}

	// UK + desktop = no match (device doesn't match)
	result = targeting.Evaluate(rules, targeting.Request{Geo: "UK", Device: "desktop"})
	if result.Matched {
		t.Error("UK + desktop should NOT match (device excluded)")
	}
}

func TestEvaluate_SegmentOR(t *testing.T) {
	rules := targeting.Rules{
		Include: targeting.TargetingSet{
			Segments: []string{"sports_fans", "tech_enthusiasts"},
		},
	}

	// User has sports_fans = match (OR within dimension)
	result := targeting.Evaluate(rules, targeting.Request{
		Segments: []string{"sports_fans", "news_readers"},
	})
	if !result.Matched {
		t.Error("user with sports_fans should match (OR within segments)")
	}

	// User has neither = no match
	result = targeting.Evaluate(rules, targeting.Request{
		Segments: []string{"food_lovers"},
	})
	if result.Matched {
		t.Error("user with food_lovers only should NOT match")
	}
}

func TestEvaluate_Exclusion(t *testing.T) {
	rules := targeting.Rules{
		Include: targeting.TargetingSet{
			Geo: []string{"UK", "DE", "FR"},
		},
		Exclude: targeting.TargetingSet{
			Segments: []string{"existing_customers"},
		},
	}

	// UK + not existing customer = match
	result := targeting.Evaluate(rules, targeting.Request{
		Geo:      "UK",
		Segments: []string{"sports_fans"},
	})
	if !result.Matched {
		t.Error("UK + sports_fans should match (not excluded)")
	}

	// UK + existing customer = excluded
	result = targeting.Evaluate(rules, targeting.Request{
		Geo:      "UK",
		Segments: []string{"sports_fans", "existing_customers"},
	})
	if result.Matched {
		t.Error("UK + existing_customers should be EXCLUDED")
	}
	if result.FailedDimension != "segments" {
		t.Errorf("failed dimension = %q, want 'segments'", result.FailedDimension)
	}
}

func TestEvaluate_GeoExclusion(t *testing.T) {
	rules := targeting.Rules{
		Include: targeting.TargetingSet{Geo: []string{"UK"}},
		Exclude: targeting.TargetingSet{Geo: []string{"UK_scotland"}},
	}

	// UK_london = match (UK included, not Scotland)
	result := targeting.Evaluate(rules, targeting.Request{Geo: "UK_london"})
	if !result.Matched {
		t.Error("UK_london should match (UK included, Scotland excluded)")
	}

	// UK_scotland = excluded
	result = targeting.Evaluate(rules, targeting.Request{Geo: "UK_scotland"})
	if result.Matched {
		t.Error("UK_scotland should be excluded")
	}
}

func TestEvaluate_DomainExclusion(t *testing.T) {
	rules := targeting.Rules{
		Exclude: targeting.TargetingSet{
			Domains: []string{"competitor.com"},
		},
	}

	result := targeting.Evaluate(rules, targeting.Request{Domain: "competitor.com"})
	if result.Matched {
		t.Error("competitor.com should be excluded")
	}

	result = targeting.Evaluate(rules, targeting.Request{Domain: "news.com"})
	if !result.Matched {
		t.Error("news.com should NOT be excluded")
	}
}

func TestApplyModifiers_Basic(t *testing.T) {
	mods := targeting.Modifiers{
		Device:     map[string]float64{"mobile": 20},
		GeoCountry: map[string]float64{"UK": 15},
	}
	ctx := targeting.ModifierContext{
		Device:     "mobile",
		GeoCountry: "UK",
	}

	adjusted, multiplier := targeting.ApplyModifiers(2.00, mods, ctx)

	// 2.00 * 1.20 * 1.15 = 2.76
	if adjusted < 2.75 || adjusted > 2.77 {
		t.Errorf("adjusted = %f, want ~2.76", adjusted)
	}
	if multiplier < 1.37 || multiplier > 1.39 {
		t.Errorf("multiplier = %f, want ~1.38", multiplier)
	}
}

func TestApplyModifiers_Negative(t *testing.T) {
	mods := targeting.Modifiers{
		Device: map[string]float64{"tablet": -30},
	}
	ctx := targeting.ModifierContext{Device: "tablet"}

	adjusted, _ := targeting.ApplyModifiers(2.00, mods, ctx)
	// 2.00 * 0.70 = 1.40
	if adjusted < 1.39 || adjusted > 1.41 {
		t.Errorf("adjusted = %f, want ~1.40", adjusted)
	}
}

func TestApplyModifiers_MaxCombinedCap(t *testing.T) {
	mods := targeting.Modifiers{
		Device:      map[string]float64{"mobile": 200},
		GeoCountry:  map[string]float64{"UK": 200},
		MaxCombined: 300,
	}
	ctx := targeting.ModifierContext{Device: "mobile", GeoCountry: "UK"}

	adjusted, _ := targeting.ApplyModifiers(2.00, mods, ctx)
	// Without cap: 2.00 * 3.00 * 3.00 = 18.00
	// With 300% max combined: 2.00 * 4.00 = 8.00
	if adjusted > 8.01 {
		t.Errorf("adjusted = %f, should be capped at 8.00 (300%% max)", adjusted)
	}
}

func TestApplyModifiers_NoModifiers(t *testing.T) {
	mods := targeting.Modifiers{}
	ctx := targeting.ModifierContext{Device: "mobile"}

	adjusted, multiplier := targeting.ApplyModifiers(2.00, mods, ctx)
	if adjusted != 2.00 || multiplier != 1.0 {
		t.Errorf("no modifiers: adjusted = %f (want 2.00), multiplier = %f (want 1.0)", adjusted, multiplier)
	}
}

func TestApplyModifiers_AudienceSegment(t *testing.T) {
	mods := targeting.Modifiers{
		Audience: map[string]float64{
			"existing_customers": 50,
			"high_value":         30,
		},
	}
	ctx := targeting.ModifierContext{
		Segments: []string{"existing_customers", "high_value"},
	}

	// Should use the highest matching modifier (50%)
	adjusted, _ := targeting.ApplyModifiers(2.00, mods, ctx)
	// 2.00 * 1.50 = 3.00
	if adjusted < 2.99 || adjusted > 3.01 {
		t.Errorf("adjusted = %f, want ~3.00 (highest segment modifier)", adjusted)
	}
}

func TestApplyModifiers_TimeOfDay(t *testing.T) {
	mods := targeting.Modifiers{
		TimeOfDay: []targeting.TimeModifier{
			{StartHour: 9, EndHour: 17, Modifier: 10}, // business hours +10%
			{StartHour: 0, EndHour: 6, Modifier: -30}, // night -30%
		},
	}

	// 14:00 = business hours
	ctx := targeting.ModifierContext{HourOfDay: 14}
	adjusted, _ := targeting.ApplyModifiers(2.00, mods, ctx)
	if adjusted < 2.19 || adjusted > 2.21 {
		t.Errorf("business hours: adjusted = %f, want ~2.20", adjusted)
	}

	// 03:00 = night
	ctx = targeting.ModifierContext{HourOfDay: 3}
	adjusted, _ = targeting.ApplyModifiers(2.00, mods, ctx)
	if adjusted < 1.39 || adjusted > 1.41 {
		t.Errorf("night: adjusted = %f, want ~1.40", adjusted)
	}
}
