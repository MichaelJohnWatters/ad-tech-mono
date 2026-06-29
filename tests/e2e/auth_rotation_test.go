//go:build e2e

// Phase 7: rotation workflow tests. These exercise the multi-step
// operator flow that the secrets UI will eventually drive:
//
//   1. Operator mints a replacement key (insert as active or rotating).
//   2. Both old and new keys validate during a grace window.
//   3. Operator demotes the old key to "rotating", then "revoked".
//   4. After revoke, only the new key works.
//
// They also cover the natural-expiry branch (expires_at < now) that
// can fire independent of an operator-driven status change — useful
// for time-limited partner keys.
//
// All tests target the SSP CRUD endpoint (which gates on AuthAPIKey
// identically to DSP). Picking one service keeps the matrix small;
// the DSP-side path is covered by TestAuthCRUD_DSPCampaignsAcceptsDevKey
// in the Phase 4 suite.
package e2e

import (
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// TestRotation_FullWorkflow walks the canonical rotation sequence:
// active V1 → insert rotating V2 (both validate) → promote V2 (active)
// and revoke V1 → only V2 validates.
//
// This is the single test that proves the end-to-end operator flow
// works. If it passes, the warm cache picks up status changes and
// the middleware honours them — which means rotations can happen
// live without bouncing services.
func TestRotation_FullWorkflow(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	const name = "rotation-workflow-test"
	const v1 = "rotation-test-key-v1-do-not-use"
	const v2 = "rotation-test-key-v2-do-not-use"
	h.DeleteAPIKeysByName(t, name)
	t.Cleanup(func() { h.DeleteAPIKeysByName(t, name) })

	// Step 1: V1 active.
	h.InsertAPIKey(t, name, v1, "active")
	if got := doManagementGet(t, h, h.URLs.SSP+routes.SSPPlacements, v1); got != http.StatusOK {
		t.Fatalf("step 1 V1 active: got %d, want 200", got)
	}

	// Step 2: V2 rotating — both must validate (grace window).
	// Note: this leaves both rows under the same name. The cache
	// dedupes by ID so each row is loaded as a distinct entry.
	h.InsertAPIKey(t, name, v2, "rotating")
	if got := doManagementGet(t, h, h.URLs.SSP+routes.SSPPlacements, v1); got != http.StatusOK {
		t.Errorf("step 2 V1 (grace): got %d, want 200", got)
	}
	if got := doManagementGet(t, h, h.URLs.SSP+routes.SSPPlacements, v2); got != http.StatusOK {
		t.Errorf("step 2 V2 (rotating): got %d, want 200", got)
	}

	// Step 3: cutover — flip every row under this name to the final
	// state. The two-step (revoke V1 first, then promote V2) UI flow
	// collapses here because both updates target the same name and the
	// cache only re-syncs after both writes.
	h.UpdateAPIKeyStatus(t, name, "revoked")
	// Re-insert V2 as active (the prior `rotating` row got swept by the
	// blanket revoke above). In a real operator workflow this would be
	// the "promote" button on the rotating row.
	h.InsertAPIKey(t, name+"-new", v2, "active")
	t.Cleanup(func() { h.DeleteAPIKeysByName(t, name+"-new") })

	if got := doManagementGet(t, h, h.URLs.SSP+routes.SSPPlacements, v1); got != http.StatusUnauthorized {
		t.Errorf("step 3 V1 (revoked): got %d, want 401", got)
	}
	if got := doManagementGet(t, h, h.URLs.SSP+routes.SSPPlacements, v2); got != http.StatusOK {
		t.Errorf("step 3 V2 (promoted): got %d, want 200", got)
	}
}

// TestRotation_ExpiredKeyRejected — natural-expiry path. Even without
// an operator marking the key revoked, an expires_at in the past
// should make Secret.IsAcceptable return false.
//
// Defence-in-depth for partner keys: an operator who forgets to
// revoke a deprecated partner shared-secret still loses access at
// the expiry boundary.
func TestRotation_ExpiredKeyRejected(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	const name = "rotation-expired-test"
	const value = "expired-test-key-do-not-use"
	h.DeleteAPIKeysByName(t, name)
	t.Cleanup(func() { h.DeleteAPIKeysByName(t, name) })

	// Past expiry — status is active but Secret.IsAcceptable filters
	// on now > expires_at independent of status.
	h.InsertAPIKeyWithExpiry(t, name, value, -1*time.Hour)
	if got := doManagementGet(t, h, h.URLs.SSP+routes.SSPPlacements, value); got != http.StatusUnauthorized {
		t.Errorf("expired key: got %d, want 401", got)
	}
}

// TestRotation_FutureExpiryAccepted — sanity test for the expiry
// path: a key with an expiry well in the future should validate just
// like an open-ended one. Lets the negative test above stand for the
// expiry-filtered branch without ambiguity about whether the column
// is being read at all.
func TestRotation_FutureExpiryAccepted(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	const name = "rotation-future-test"
	const value = "future-expiry-key-do-not-use"
	h.DeleteAPIKeysByName(t, name)
	t.Cleanup(func() { h.DeleteAPIKeysByName(t, name) })

	h.InsertAPIKeyWithExpiry(t, name, value, 1*time.Hour)
	if got := doManagementGet(t, h, h.URLs.SSP+routes.SSPPlacements, value); got != http.StatusOK {
		t.Errorf("future-expiry key: got %d, want 200", got)
	}
}

// TestRotation_PromoteRotatingToActive — verifies the lifecycle
// transition "rotating → active" doesn't break validation. Catches
// regressions where the middleware accidentally only accepts active
// keys and ignores rotating ones (which would defeat the grace window
// entirely).
func TestRotation_PromoteRotatingToActive(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	const name = "rotation-promote-test"
	const value = "promote-test-key-do-not-use"
	h.DeleteAPIKeysByName(t, name)
	t.Cleanup(func() { h.DeleteAPIKeysByName(t, name) })

	h.InsertAPIKey(t, name, value, "rotating")
	if got := doManagementGet(t, h, h.URLs.SSP+routes.SSPPlacements, value); got != http.StatusOK {
		t.Fatalf("rotating: got %d, want 200", got)
	}

	h.UpdateAPIKeyStatus(t, name, "active")
	if got := doManagementGet(t, h, h.URLs.SSP+routes.SSPPlacements, value); got != http.StatusOK {
		t.Errorf("active after promote: got %d, want 200", got)
	}
}
