//go:build e2e

// Config manager + observability tests. The config tests need a harness
// helper that writes through the config manager API (which exists in
// cmd/gateway). Observability tests need a way to read Jaeger spans or
// at minimum assert traceparent header propagation.
package e2e

import "testing"

// --- Config -----------------------------------------------------------------

func TestConfigLiveChangePropagates(t *testing.T) {
	t.Skip("needs harness.SetConfig(key, value) helper that POSTs to gateway config API + waits for next poll; pending")
}

func TestConfigPerPodOverrideApplied(t *testing.T) {
	t.Skip("per-pod override (dsp.pod-internal-0.foo) — same harness gap as above; pending")
}

func TestConfigInvalidValueRejected(t *testing.T) {
	t.Skip("schema validation rejection — needs the config-write helper; pending")
}

// --- Observability ----------------------------------------------------------

func TestTracePropagatesSSPToExchangeToDSP(t *testing.T) {
	t.Skip("would assert by reading Jaeger /api/traces; pending a Jaeger client wrapper in the harness")
}

func TestMetricsEndpointPrometheusFormat(t *testing.T) {
	t.Skip("simple GET on /metrics and assert text/plain + counter format; quick to add — pending")
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
