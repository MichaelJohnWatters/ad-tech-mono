//go:build e2e

package harness

import "testing"

// SetOptOut writes (or updates) a row in the global opt_out_registry for a
// user at the given level (1=no personalisation, 2=no tracking, 3=full
// deletion). opt_out_registry is a platform-wide table keyed by user_id —
// no tenant scope — so this uses the plain DB handle, not WithTenant.
//
// The DSP enforces opt-outs from a warm cache, so callers should
// RefreshAllCaches (or PublishInvalidate on the opt-outs subject) after
// this for the change to take effect on the bid path.
func (h *Harness) SetOptOut(t *testing.T, userID string, level int) {
	t.Helper()
	const q = `
INSERT INTO opt_out_registry (user_id, level, source, requested_at)
VALUES ($1, $2, 'e2e', now())
ON CONFLICT (user_id) DO UPDATE SET level = EXCLUDED.level, requested_at = now()`
	if _, err := h.DB.Exec(q, userID, level); err != nil {
		t.Fatalf("set opt-out for %s: %v", userID, err)
	}
}

// ClearOptOut removes a user's opt-out row. Used to reset between cases.
func (h *Harness) ClearOptOut(t *testing.T, userID string) {
	t.Helper()
	if _, err := h.DB.Exec(`DELETE FROM opt_out_registry WHERE user_id = $1`, userID); err != nil {
		t.Fatalf("clear opt-out for %s: %v", userID, err)
	}
}
