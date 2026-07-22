package keys

import (
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
)

// Schema sizes are pinned so an accidental deletion (or a silently-failed
// merge) shows up as a test failure, not a key that quietly stops being
// registered. Add a key -> bump the count.
func TestSchemaSizes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []config.SchemaEntry
		want    int
	}{
		{"reporting", ReportingSchema(), 24},
		{"report-runner", ReportRunnerSchema(), 13},
		{"dsp", DSPSchema(), 21},
		{"exchange", ExchangeSchema(), 21},
		{"gateway", GatewaySchema(), 11},
		{"tracker", TrackerSchema(), 11},
		{"adserver", AdServerSchema(), 10},
		{"ssp", SSPSchema(), 10},
		{"ssai", SSAISchema(), 16},
		{"transcoder", TranscoderSchema(), 5},
		{"webhooks", WebhooksSchema(), 5},
		{"notifications", NotificationsSchema(), 2},
		{"identity-consumer", IdentityConsumerSchema(), 10},
		{"publisher-adserver", PublisherAdServerSchema(), 9},
		{"content-packager", ContentPackagerSchema(), 9},
		{"prewarm", PrewarmSchema(), 2},
		{"pipeline", PipelineSchema(), 10},
	} {
		if len(tc.entries) != tc.want {
			t.Errorf("%s schema: %d entries, want %d", tc.name, len(tc.entries), tc.want)
		}
		for _, e := range tc.entries {
			// Service may be blank for shared entries (Setup fills it in).
			if e.Key == "" || e.Type == "" || e.Tier == "" || e.Description == "" {
				t.Errorf("%s schema: incomplete entry %+v", tc.name, e)
			}
		}
	}
}

// No key may be declared in two sets — a key's schema has exactly one home.
func TestNoDuplicateKeysAcrossSets(t *testing.T) {
	all := map[string]string{}
	for name, entries := range map[string][]config.SchemaEntry{
		"reporting":          ReportingSchema(),
		"report-runner":      ReportRunnerSchema(),
		"dsp":                DSPSchema(),
		"exchange":           ExchangeSchema(),
		"gateway":            GatewaySchema(),
		"tracker":            TrackerSchema(),
		"adserver":           AdServerSchema(),
		"ssp":                SSPSchema(),
		"ssai":               SSAISchema(),
		"transcoder":         TranscoderSchema(),
		"webhooks":           WebhooksSchema(),
		"notifications":      NotificationsSchema(),
		"identity-consumer":  IdentityConsumerSchema(),
		"publisher-adserver": PublisherAdServerSchema(),
		"content-packager":   ContentPackagerSchema(),
		"prewarm":            PrewarmSchema(),
	} {
		for _, e := range entries {
			if e.Service == "" {
				continue // shared-by-design (transcodeSharedSet); Setup fills the service
			}
			if prev, ok := all[e.Key]; ok {
				t.Errorf("key %q declared in both %s and %s", e.Key, prev, name)
			}
			all[e.Key] = name
		}
	}
}

// Spot-check that typed defaults parsed from the schema strings are sane.
func TestSpotDefaults(t *testing.T) {
	cfg := config.Load()
	if got := Reporting.QueryTimeout.Get(cfg); got != 2*time.Minute {
		t.Errorf("reporting.query_timeout default: %v", got)
	}
	if got := DSP.BudgetResetInterval.Get(cfg); got != 24*time.Hour {
		t.Errorf("dsp.budget_reset_interval default: %v", got)
	}
	if !Reporting.ClickHouseBatchConsumer.Get(cfg) {
		t.Error("reporting.clickhouse_batch_consumer default should be true (runtime truth; schema previously drifted to false)")
	}
	if got := Reporting.QueryTimeout.Key(); got != "reporting.query_timeout" {
		t.Errorf("Key(): %q", got)
	}
}
