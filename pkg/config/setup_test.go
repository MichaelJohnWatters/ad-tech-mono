package config

import "testing"

// TestWithSeedDefaults_OverridesSchemaDefault — proves WithSeedDefaults
// changes the seed value used when writing the per-pod row at Register
// time. The override only mutates the SchemaEntry copies used in the
// Register loop; the in-process schema registry keeps the original
// default for Validate / UI display.
//
// We don't run full Setup() here because it requires Postgres + NATS;
// instead we apply the same logic Setup does to fullSchema and assert
// the override fired.
func TestWithSeedDefaults_OverridesSchemaDefault(t *testing.T) {
	schema := []SchemaEntry{
		{Key: "dsp.noise_pct", Type: "float", Tier: TierLive, Default: "0", Service: "dsp"},
		{Key: "dsp.no_bid_rate", Type: "float", Tier: TierLive, Default: "0", Service: "dsp"},
		{Key: "dsp.daily_budget_default", Type: "float", Tier: TierLive, Default: "1000", Service: "dsp"},
	}

	o := &setupOpts{}
	WithSeedDefaults(map[string]string{
		"dsp.noise_pct":   "30",
		"dsp.no_bid_rate": "0.20",
	})(o)

	// Mirror what Setup does in its fullSchema construction loop.
	got := make(map[string]string)
	for _, e := range schema {
		if v, ok := o.seedDefaults[e.Key]; ok {
			e.Default = v
		}
		got[e.Key] = e.Default
	}

	if got["dsp.noise_pct"] != "30" {
		t.Errorf("noise_pct default = %q, want 30", got["dsp.noise_pct"])
	}
	if got["dsp.no_bid_rate"] != "0.20" {
		t.Errorf("no_bid_rate default = %q, want 0.20", got["dsp.no_bid_rate"])
	}
	// Unrelated keys must keep their schema defaults.
	if got["dsp.daily_budget_default"] != "1000" {
		t.Errorf("daily_budget_default = %q, want 1000 (unchanged)", got["dsp.daily_budget_default"])
	}
}

// TestWithSeedDefaults_NilMapIsNoop — passing a nil/empty override map
// must leave the schema defaults intact. Used by the DSP main when the
// YAML profile isn't found (e.g. tests running outside profiles/dsps/).
func TestWithSeedDefaults_NilMapIsNoop(t *testing.T) {
	schema := []SchemaEntry{
		{Key: "dsp.noise_pct", Type: "float", Tier: TierLive, Default: "0", Service: "dsp"},
	}

	o := &setupOpts{}
	WithSeedDefaults(nil)(o)

	for _, e := range schema {
		if v, ok := o.seedDefaults[e.Key]; ok {
			e.Default = v
		}
		if e.Default != "0" {
			t.Errorf("default = %q, want 0 (nil override should be no-op)", e.Default)
		}
	}
}
