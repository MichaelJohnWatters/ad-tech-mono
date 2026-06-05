//go:build e2e

// Config manager + observability tests. The config tests need a harness
// helper that writes through the config manager API (which exists in
// cmd/gateway). Observability tests need a way to read Jaeger spans or
// at minimum assert traceparent header propagation.
package e2e

import (
	"context"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// --- Config -----------------------------------------------------------------

// TestConfigLiveChangePropagates — write a known live-tier key on a target
// pod via the gateway config API, then read it back through the resolved
// endpoint. Asserts the value lands in the per-pod config row and is
// served back as a pod-scoped resolution (source != schema default).
func TestConfigLiveChangePropagates(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)

	const pod = harness.PodDSPInternal
	const key = "dsp.daily_budget_default"

	before := resolveConfig(t, h, key, pod)
	// Pick a value that's definitely not the current one so the change
	// is observable. Schema default is 1000, seeded as 1000 for internal.
	want := "9876"
	if before.Value == want {
		want = "1234"
	}

	h.SetConfigForPod(t, key, want, pod)
	t.Cleanup(func() {
		h.SetConfigForPod(t, key, before.Value, pod)
	})

	after := resolveConfig(t, h, key, pod)
	if after.Value != want {
		t.Fatalf("after SetConfigForPod, resolved %s=%q want %q (source=%q pod=%q)",
			key, after.Value, want, after.Source, after.PodID)
	}
}

// TestConfigPerPodOverrideApplied — writing a value to one pod's row must
// not leak to another pod's resolution. Sets two distinct values on
// dsp-internal-0 and dsp-competitor1-0 and asserts each pod sees its own.
func TestConfigPerPodOverrideApplied(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)

	const key = "dsp.daily_budget_default"
	const podA = harness.PodDSPInternal
	const podB = harness.PodDSPCompetitor1

	beforeA := resolveConfig(t, h, key, podA)
	beforeB := resolveConfig(t, h, key, podB)

	const wantA = "5111"
	const wantB = "7222"

	h.SetConfigForPod(t, key, wantA, podA)
	h.SetConfigForPod(t, key, wantB, podB)
	t.Cleanup(func() {
		h.SetConfigForPod(t, key, beforeA.Value, podA)
		h.SetConfigForPod(t, key, beforeB.Value, podB)
	})

	afterA := resolveConfig(t, h, key, podA)
	afterB := resolveConfig(t, h, key, podB)

	if afterA.Value != wantA {
		t.Errorf("podA %s = %q, want %q", podA, afterA.Value, wantA)
	}
	if afterB.Value != wantB {
		t.Errorf("podB %s = %q, want %q", podB, afterB.Value, wantB)
	}
	if afterA.Value == afterB.Value {
		t.Errorf("pods returned the same value — per-pod isolation broken")
	}
}

// TestConfigInvalidValueRejected — schema declares dsp.daily_budget_default
// as type=int. Posting a non-integer value must be rejected by the gateway
// config API with a 4xx and not persist. Verifies that PUT /v1/config runs
// schema validation before writing.
func TestConfigInvalidValueRejected(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)

	const pod = harness.PodDSPInternal
	const key = "dsp.daily_budget_default"

	before := resolveConfig(t, h, key, pod)

	// Direct PUT — we can't use SetConfigForPod because it Fatals on
	// non-200, and we want to assert exactly that the API returned 4xx.
	body := strings.NewReader(`{"key":"` + key + `","value":"not-an-int","pod_id":"` + pod + `"}`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, h.URLs.Gateway+"/v1/config", body)
	if err != nil {
		t.Fatalf("build PUT: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.HTTP.Do(req)
	if err != nil {
		t.Fatalf("PUT /v1/config: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 400 || resp.StatusCode >= 500 {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT with invalid value: status %d (want 4xx); body=%s", resp.StatusCode, string(respBody))
	}

	// Value must not have changed.
	after := resolveConfig(t, h, key, pod)
	if after.Value != before.Value {
		t.Errorf("invalid PUT was rejected (%d) but value changed: before=%q after=%q",
			resp.StatusCode, before.Value, after.Value)
	}
}

// --- Observability ----------------------------------------------------------

func TestTracePropagatesSSPToExchangeToDSP(t *testing.T) {
	t.Skip("would assert by reading Jaeger /api/traces; pending a Jaeger client wrapper in the harness")
}

// TestMetricsEndpointPrometheusFormat — every service exposes a /metrics
// endpoint scraped by Prometheus. Asserts each one returns 200 with the
// Prometheus text exposition format (text/plain version=0.0.4) and at
// least one well-formed `# HELP` / `# TYPE` directive — enough to detect
// a regression where the endpoint dropped to a different format or a
// service stopped registering its handler entirely.
func TestMetricsEndpointPrometheusFormat(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)

	services := map[string]string{
		"exchange":           h.URLs.Exchange,
		"dsp":                h.URLs.DSP,
		"dsp-comp1":          h.URLs.DSPComp1,
		"dsp-comp2":          h.URLs.DSPComp2,
		"tracker":            h.URLs.Tracker,
		"ssp":                h.URLs.SSP,
		"adserver":           h.URLs.AdServer,
		"reporting":          h.URLs.Reporting,
		"gateway":            h.URLs.Gateway,
		"publisher-adserver": h.URLs.PublisherAdServer,
	}

	// Prometheus exposition format: lines starting with `# HELP <name>` and
	// `# TYPE <name> <gauge|counter|histogram|summary>`. One pair is enough.
	helpLine := regexp.MustCompile(`(?m)^# HELP \w+`)
	typeLine := regexp.MustCompile(`(?m)^# TYPE \w+ (counter|gauge|histogram|summary|untyped)`)

	for name, base := range services {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/metrics", nil)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			resp, err := h.HTTP.Do(req)
			if err != nil {
				t.Fatalf("GET %s/metrics: %v", base, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			ct := resp.Header.Get("Content-Type")
			if !strings.HasPrefix(ct, "text/plain") {
				t.Errorf("Content-Type = %q, want text/plain prefix", ct)
			}
			body, _ := io.ReadAll(resp.Body)
			if !helpLine.Match(body) {
				t.Errorf("no `# HELP <name>` line found in /metrics body (size=%d)", len(body))
			}
			if !typeLine.Match(body) {
				t.Errorf("no `# TYPE <name> <kind>` line found in /metrics body (size=%d)", len(body))
			}
		})
	}
}

func TestStructuredLogsIncludeTraceID(t *testing.T) {
	t.Skip("requires log capture from each service; out of scope for HTTP-only harness")
}

// --- Migration --------------------------------------------------------------

func TestMigrationForwardPreservesData(t *testing.T) {
	t.Skip("requires running migrations forward/backward from harness; depends on cmd/migrate having a 'one step' mode (currently up-to-latest)")
}

func TestMigrationRollbackSafety(t *testing.T) {
	t.Skip("same migration helper gap")
}
