//go:build e2e

// Seed-default propagation. Verifies the WithSeedDefaults fix: the YAML
// profile values for competitor DSPs land in Postgres for their pod_id
// at boot, instead of being shadowed by the schema-wide 0 default.
//
// Pre-fix behaviour: competitor1.yaml said noise_pct=30, no_bid_rate=0.20
// but registry.Register seeded (pod_id=dsp-competitor1, key=dsp.noise_pct,
// value=0) using the schema default. Operator-modified values would still
// have shown the right number, but on a fresh boot competitors were
// silently deterministic. WithDeterministicCompetitors was a no-op.
//
// This test queries the gateway's /v1/config?resolved=true endpoint with
// pod=dsp-competitor1 to assert the effective value Postgres holds is now
// 30 (matches YAML) — and the internal DSP stays at 0 (matches both YAML
// and schema for internal).
package e2e

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

type resolvedConfig struct {
	Value  string `json:"value"`
	Source string `json:"source"`
	PodID  string `json:"pod_id"`
}

func resolveConfig(t *testing.T, h *harness.Harness, key, pod string) resolvedConfig {
	t.Helper()
	url := h.URLs.Gateway + routes.Config + "?resolved=true&key=" + key + "&pod=" + pod
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := h.HTTP.Do(req)
	if err != nil {
		t.Fatalf("config get %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var out resolvedConfig
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode resolved config: %v\nbody: %s", err, string(body))
	}
	return out
}

// TestSeedDefaults_CompetitorDSPGetsYAMLNoise — the fix's primary assertion.
// After DSP boots with WithSeedDefaults, the per-pod config row for
// dsp-competitor1 / dsp.noise_pct must hold 30 (the YAML value), not 0
// (the schema default).
func TestSeedDefaults_CompetitorDSPGetsYAMLNoise(t *testing.T) {
	_ = harness.WaitReady(t, 60*time.Second)
	h := harness.New(t)

	c1Noise := resolveConfig(t, h, "dsp.noise_pct", "dsp-competitor1")
	if c1Noise.Value != "30" {
		t.Errorf("dsp-competitor1 noise_pct = %q, want 30 (YAML default); source=%q pod=%q",
			c1Noise.Value, c1Noise.Source, c1Noise.PodID)
	}

	c1NoBid := resolveConfig(t, h, "dsp.no_bid_rate", "dsp-competitor1")
	if c1NoBid.Value != "0.2" {
		t.Errorf("dsp-competitor1 no_bid_rate = %q, want 0.2 (YAML default); source=%q",
			c1NoBid.Value, c1NoBid.Source)
	}
}

// TestSeedDefaults_Comp2HigherNoiseLanded — same check for competitor2,
// whose YAML says noise_pct=40 and no_bid_rate=0.15. Two-profile coverage
// makes sure the override map iteration order doesn't accidentally apply
// the same value to both pods.
func TestSeedDefaults_Comp2HigherNoiseLanded(t *testing.T) {
	_ = harness.WaitReady(t, 60*time.Second)
	h := harness.New(t)

	c2Noise := resolveConfig(t, h, "dsp.noise_pct", "dsp-competitor2")
	if c2Noise.Value != "40" {
		t.Errorf("dsp-competitor2 noise_pct = %q, want 40 (YAML default)", c2Noise.Value)
	}

	c2NoBid := resolveConfig(t, h, "dsp.no_bid_rate", "dsp-competitor2")
	if c2NoBid.Value != "0.15" {
		t.Errorf("dsp-competitor2 no_bid_rate = %q, want 0.15 (YAML default)", c2NoBid.Value)
	}
}

// TestSeedDefaults_InternalDSPStaysZero — sanity check that the override
// only affects pods whose YAML differs from the schema default. Internal
// DSP has noise_pct=0 in YAML AND schema, so it stays at 0.
func TestSeedDefaults_InternalDSPStaysZero(t *testing.T) {
	_ = harness.WaitReady(t, 60*time.Second)
	h := harness.New(t)

	r := resolveConfig(t, h, "dsp.noise_pct", "dsp-internal-0")
	if r.Value != "0" {
		t.Errorf("dsp-internal-0 noise_pct = %q, want 0 (matches both YAML and schema)", r.Value)
	}
}
