//go:build e2e

// Chaos tests — fail-mode behavior when an infra dep is unhealthy. These
// require manipulating the Tilt-managed pods (kubectl delete pod -l app=X),
// which the current harness doesn't do. Each test sketches the assertion
// and is skipped until a ChaosKill helper lands.
//
// Workaround for the user: the Tiltfile already has `chaos-kill-redis`
// and `chaos-kill-nats` buttons (manual trigger) — running those + the
// matching e2e test by hand exercises the same path. Eventually the
// helpers below will wrap the same kubectl call.
package e2e

import "testing"

func TestChaosNATSDownTrackerHTTPFallback(t *testing.T) {
	t.Skip("requires harness.ChaosKillNATS helper that runs `kubectl delete pod -l app=nats`; pending")
}

func TestChaosRedisDownBudgetFailOpen(t *testing.T) {
	t.Skip("requires harness.ChaosKillRedis helper; pending. When wired, asserts: DSP keeps bidding (fail-open per policy), warns in logs")
}

func TestChaosPostgresDownCachesServeStaleSnapshots(t *testing.T) {
	t.Skip("requires harness.ChaosKillPostgres + a fresh service restart to test the cold-start behavior; pending")
}

func TestChaosMinioDownCreativeServeUsesHTMLContentOnly(t *testing.T) {
	t.Skip("requires harness.ChaosKillMinio; verifies that html_content-only creatives serve fine while asset_url ones fail clean; pending")
}
