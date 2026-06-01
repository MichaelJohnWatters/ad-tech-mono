//go:build e2e

package harness

import (
	"database/sql"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/idgen"
)

// CreateSegment inserts an audience_segments row owned by the given account
// and returns the derived UUID. Defaults visibility to 'public' so the SSP
// stamps it on outbound bid requests (path A). For DSP-private segments use
// CreateDSPPrivateSegment instead. Idempotent via ON CONFLICT on the
// deterministic ID — re-running the test reuses the same row after Reset.
//
// Type defaults to "first_party" since that's what direct-loaded test
// memberships effectively are (an advertiser asserting "these users are
// mine"). Status is "active" so the SSP lookup picks them up immediately.
func (h *Harness) CreateSegment(t *testing.T, owner Account, externalKey string) string {
	t.Helper()
	return h.createSegment(t, owner, externalKey, "public")
}

// CreateDSPPrivateSegment inserts a segment with visibility='dsp_private',
// which the SSP filter excludes from user.ext.segments. The DSP reads it
// directly via DSPSegmentsForUser. Used by TestTargetingDSPPrivateSegment
// to prove enrichment happens on the DSP side, not via the bid request.
func (h *Harness) CreateDSPPrivateSegment(t *testing.T, owner Account, externalKey string) string {
	t.Helper()
	return h.createSegment(t, owner, externalKey, "dsp_private")
}

func (h *Harness) createSegment(t *testing.T, owner Account, externalKey, visibility string) string {
	t.Helper()
	id := idgen.Derive("segment", externalKey)
	h.WithTenant(t, owner.ID, func(tx *sql.Tx) {
		const q = `
INSERT INTO audience_segments (id, account_id, name, type, status, source, visibility, created_at, updated_at)
VALUES ($1, $2, $3, 'first_party', 'active', 'e2e_harness', $4, now(), now())
ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, visibility = EXCLUDED.visibility, updated_at = now()`
		if _, err := tx.Exec(q, id, owner.ID, externalKey, visibility); err != nil {
			t.Fatalf("createSegment(%s, %s): %v", externalKey, visibility, err)
		}
	})
	return id
}

// AddUserToSegment links a user_id to a segment. Mirrors the production write
// path the audience pipeline (CRM import, behavioural rollup, lookalike model
// publish) will eventually use. Idempotent via PK conflict.
//
// account_id on the membership row must match the segment's owner so the RLS
// policy on audience_segment_members admits the INSERT under tenant context.
func (h *Harness) AddUserToSegment(t *testing.T, segmentID, userID string) {
	t.Helper()
	var accountID string
	if err := h.DB.QueryRow(
		"SELECT account_id::text FROM audience_segments WHERE id = $1",
		segmentID,
	).Scan(&accountID); err != nil {
		t.Fatalf("AddUserToSegment: lookup segment %s owner: %v", segmentID, err)
	}
	h.WithTenant(t, accountID, func(tx *sql.Tx) {
		const q = `
INSERT INTO audience_segment_members (segment_id, user_id, account_id, added_at)
VALUES ($1, $2, $3, now())
ON CONFLICT (segment_id, user_id) DO NOTHING`
		if _, err := tx.Exec(q, segmentID, userID, accountID); err != nil {
			t.Fatalf("AddUserToSegment(%s, %s): %v", segmentID, userID, err)
		}
	})
}
