//go:build e2e

// Publisher placement status lifecycle — locks the fix for the "Pause button does
// nothing" bug. The publisher-portal Pause/Resume toggle PATCHes a placement's
// status to 'paused'/'active', and the archive path uses 'archived'. The original
// placements_status_check (migration 008) only allowed ('active','inactive'), so
// 'paused' hit a CHECK violation → the SSP handler 500'd → the button silently
// failed. Migration 106 widened the constraint. This is the exact gap the suite
// missed: it only ever paused publisher_line_items (whose constraint DID allow
// paused), never a placement.
package e2e

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestPlacementPauseResumeArchive(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	key := fmt.Sprintf("plstatus-%d", time.Now().UnixNano())
	owner := h.CreatePublisher(t, key)
	pub := h.AddPublisher(t, owner, key, key+".example")
	pl := h.AddPlacement(t, pub, key+"-pl", 300, 250, 0.5, nil)
	client := h.OwnerClient(t, owner.ID)

	url := h.URLs.Gateway + routes.APIPlacements + pl.ID // /v1/api/placements/{id}

	ok := func(code int) bool { return code == http.StatusNoContent || code == http.StatusOK }

	// Pause — the regression. Before migration 106 this was a 500
	// (placements_status_check rejected 'paused'); it must now succeed.
	if code, _ := partnerReq(t, client, http.MethodPatch, url, `{"status":"paused"}`); !ok(code) {
		t.Fatalf("pause placement: got %d, want 2xx (placements_status_check must allow 'paused')", code)
	}
	// Archive (the soft-delete path) must also be accepted, not 500.
	if code, _ := partnerReq(t, client, http.MethodPatch, url, `{"status":"archived"}`); !ok(code) {
		t.Fatalf("archive placement: got %d, want 2xx", code)
	}
	// Resume back to active.
	if code, _ := partnerReq(t, client, http.MethodPatch, url, `{"status":"active"}`); !ok(code) {
		t.Fatalf("resume placement: got %d, want 2xx", code)
	}
}
