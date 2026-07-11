//go:build e2e

// Chaos helpers — kill and recover the Tilt-managed infra pods so fail-mode
// e2e tests can assert graceful degradation (fail-open budget, HTTP tracker
// fallback, warm-cache serving, html_content creatives). They wrap the same
// `kubectl -n adtech delete pod` the Tiltfile's chaos-kill buttons run, plus a
// recovery wait so the stack is healthy again before the next test.
package harness

import (
	"context"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// chaosNamespace is the k8s namespace the local stack runs in.
const chaosNamespace = "adtech"

// kubectlReachable reports whether kubectl can list pods in the adtech namespace.
func kubectlReachable() bool {
	return exec.Command("kubectl", "-n", chaosNamespace, "get", "pods", "--no-headers").Run() == nil
}

// RequireKubectl skips the test unless kubectl can reach the adtech namespace.
// Chaos tests need pod-level control the plain HTTP harness can't provide, so
// against a non-k8s deployment they self-skip rather than hard-fail.
func RequireKubectl(t *testing.T) {
	t.Helper()
	if !kubectlReachable() {
		t.Skip("chaos test requires kubectl access to the adtech namespace; skipping")
	}
}

func runKubectl(args ...string) (string, error) {
	full := append([]string{"-n", chaosNamespace}, args...)
	out, err := exec.Command("kubectl", full...).CombinedOutput()
	return string(out), err
}

// ChaosKill force-deletes every pod labelled app=<app> in the adtech namespace.
// The owning Deployment/StatefulSet reschedules a replacement; the restart
// window is the outage the test exercises. --force --grace-period=0 removes the
// pod object immediately so a subsequent readiness wait matches only the new pod.
func (h *Harness) ChaosKill(t *testing.T, app string) {
	t.Helper()
	out, err := runKubectl("delete", "pod", "-l", "app="+app, "--force", "--grace-period=0")
	if err != nil {
		t.Fatalf("chaos kill %s: %v\n%s", app, err, out)
	}
	t.Logf("chaos: killed %s (%s)", app, strings.TrimSpace(out))
}

// Named wrappers for the infra the chaos tests target — readability at call sites.
func (h *Harness) ChaosKillRedis(t *testing.T)    { h.ChaosKill(t, "redis") }
func (h *Harness) ChaosKillNATS(t *testing.T)     { h.ChaosKill(t, "nats") }
func (h *Harness) ChaosKillPostgres(t *testing.T) { h.ChaosKill(t, "postgres") }
func (h *Harness) ChaosKillMinio(t *testing.T)    { h.ChaosKill(t, "minio") }

// ChaosWaitReady blocks until an app=<app> pod is Ready again. Right after a
// force-delete the replacement pod may not exist yet (`kubectl wait` returns
// "no matching resources"), so it retries until Ready or the timeout elapses.
func (h *Harness) ChaosWaitReady(t *testing.T, app string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastOut string
	var lastErr error
	for time.Now().Before(deadline) {
		lastOut, lastErr = runKubectl("wait", "--for=condition=ready", "pod", "-l", "app="+app, "--timeout=10s")
		if lastErr == nil {
			t.Logf("chaos: %s ready again", app)
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("chaos: %s did not become ready within %s: %v\n%s", app, timeout, lastErr, lastOut)
}

// WithChaos kills app, runs during() while it is unavailable, then GUARANTEES
// recovery via defer — even if during() fails an assertion — so later tests
// inherit a healthy stack. A short settle after the pod is Ready gives
// dependent services time to reconnect (warm caches resubscribe, pools redial);
// the next test's WaitReady covers any remaining lag. This is the pattern chaos
// tests should use.
func (h *Harness) WithChaos(t *testing.T, app string, during func()) {
	t.Helper()
	RequireKubectl(t)
	h.ChaosKill(t, app)
	defer func() {
		h.ChaosWaitReady(t, app, 90*time.Second)
		time.Sleep(3 * time.Second)
		// Pod-ready is NOT the same as functionally recovered for stateful
		// infra. Postgres is a StatefulSet with headless-service DNS: after a
		// force-delete+recreate the `postgres` service name doesn't resolve for
		// a beat (endpoints re-propagate, dependent pools redial, Go negative-DNS
		// cache clears). The next test's /readyz-based WaitReady doesn't catch it
		// (services fail-open on readyz), so it would inherit a stack where a
		// Postgres-backed cache refresh 500s ("lookup postgres: no such host").
		// Poll a real Postgres query until it succeeds before handing off.
		if app == "postgres" {
			h.waitPostgresReachable(t, 60*time.Second)
		}
	}()
	during()
}

// waitPostgresReachable blocks until the DSP can actually query Postgres again
// (the audience-refresh endpoint reads audience_segment_members, so it 500s
// while Postgres is unresolvable and 200s once recovered). A functional gate,
// not just pod-readiness.
func (h *Harness) waitPostgresReachable(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if postgresReachableVia(h.HTTP, h.URLs.DSP) {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("Postgres-backed services did not recover within %s after restart "+
		"(dependent query still failing) — the next test would inherit a broken stack", timeout)
}

// postgresReachableVia POSTs the DSP audience-refresh (a Postgres-backed read)
// and reports whether it succeeded. Non-fatal, for polling.
func postgresReachableVia(client *http.Client, dspBase string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, dspBase+routes.DebugAudienceRefresh, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}
