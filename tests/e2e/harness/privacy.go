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

// AddIdentityEdge seeds an identity_graph link (userID → linkedID). Used by the
// deletion test to prove cross-system purge. identity_graph is global (no
// tenant scope), so this uses the plain DB handle.
func (h *Harness) AddIdentityEdge(t *testing.T, userID, linkedID, linkType string) {
	t.Helper()
	const q = `
INSERT INTO identity_graph (user_id, linked_id, source, link_type, confidence)
VALUES ($1, $2, 'e2e', $3, 1.0)
ON CONFLICT (user_id, linked_id, source) DO NOTHING`
	if _, err := h.DB.Exec(q, userID, linkedID, linkType); err != nil {
		t.Fatalf("add identity edge %s→%s: %v", userID, linkedID, err)
	}
}

// AddIdentityEdgeWithConfidence seeds an identity_graph link at a specific
// confidence — for testing the attribution resolver's confidence floor (weak
// probabilistic links must not drive billing).
func (h *Harness) AddIdentityEdgeWithConfidence(t *testing.T, userID, linkedID, linkType string, confidence float64) {
	t.Helper()
	const q = `
INSERT INTO identity_graph (user_id, linked_id, source, link_type, confidence)
VALUES ($1, $2, 'e2e', $3, $4)
ON CONFLICT (user_id, linked_id, source) DO UPDATE SET confidence = $4`
	if _, err := h.DB.Exec(q, userID, linkedID, linkType, confidence); err != nil {
		t.Fatalf("add identity edge %s→%s @%.2f: %v", userID, linkedID, confidence, err)
	}
}

// IdentityEdgeCount counts identity_graph rows touching userID (either endpoint).
func (h *Harness) IdentityEdgeCount(t *testing.T, userID string) int {
	t.Helper()
	return h.countInt(t, `SELECT count(*) FROM identity_graph WHERE user_id = $1 OR linked_id = $1`, userID)
}

// UserSegmentMembershipCount counts audience_segment_members rows for userID
// across all tenants (the owner role bypasses RLS).
func (h *Harness) UserSegmentMembershipCount(t *testing.T, userID string) int {
	t.Helper()
	return h.countInt(t, `SELECT count(*) FROM audience_segment_members WHERE user_id = $1`, userID)
}

// OptOutCompleted reports whether a user's opt_out_registry row has completed_at set.
func (h *Harness) OptOutCompleted(t *testing.T, userID string) bool {
	t.Helper()
	return h.countInt(t, `SELECT count(*) FROM opt_out_registry WHERE user_id = $1 AND completed_at IS NOT NULL`, userID) > 0
}

// OptOutVerified reports whether a user's opt_out_registry row has verified_at set.
func (h *Harness) OptOutVerified(t *testing.T, userID string) bool {
	t.Helper()
	return h.countInt(t, `SELECT count(*) FROM opt_out_registry WHERE user_id = $1 AND verified_at IS NOT NULL`, userID) > 0
}

func (h *Harness) countInt(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := h.DB.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", q, err)
	}
	return n
}
