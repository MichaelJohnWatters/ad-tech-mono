// Package postgres is the Postgres-backed audience segment membership store.
//
// The in-memory pkg/audience.Store is fine for unit tests but loses everything
// on restart. This package implements the hot-path lookup the SSP needs —
// "given a user_id, what segments are they in?" — against the
// audience_segment_members table (migration 019).
//
// Scope is intentionally minimal for now: just SegmentsForUser. Segment
// creation and membership writes happen via direct SQL elsewhere (seed
// scripts, the test harness). When that surface grows we can add Create/Add
// methods that mirror the in-memory Store contract.
package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/lib/pq"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/idgen"
)

// Store reads audience segment memberships from Postgres.
type Store struct {
	db *sql.DB
}

// Segment is a segment row with its member count, for the management UI.
type Segment struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Type       string    `json:"type"`
	Status     string    `json:"status"`
	Source     string    `json:"source"`
	Visibility string    `json:"visibility"`
	Members    int       `json:"members"`
	UpdatedAt  time.Time `json:"updated_at"`
	// MatchRate is the fraction (0..1) of the last upload's ids resolvable
	// via identity_graph — nil until a first upload computes it.
	MatchRate    *float64   `json:"match_rate,omitempty"`
	LastUploadAt *time.Time `json:"last_upload_at,omitempty"`
	// TaxonomyID/TaxonomyPath are the optional IAB Audience Taxonomy 1.1
	// label (migration 062) — nil for custom-only segments.
	TaxonomyID   *int64  `json:"taxonomy_id,omitempty"`
	TaxonomyPath *string `json:"taxonomy_path,omitempty"`
	// DataFeeMicros is the optional data fee (migration 063): CPM in
	// micro-dollars the owner earns when this public labelled segment rides
	// a bid request an external buyer wins. nil = not monetized.
	DataFeeMicros *int64 `json:"data_fee_micros,omitempty"`
}

// ListSegments returns every segment for an account with its member count,
// most-recently-updated first. Tenant-scoped via RLS (withTenant sets
// app.current_account_id) plus an explicit account_id filter.
func (s *Store) ListSegments(ctx context.Context, accountID string) ([]Segment, error) {
	out := []Segment{}
	err := s.withTenant(ctx, accountID, func(tx *sql.Tx) error {
		const q = `
SELECT s.id::text, s.name, s.type, s.status, s.source, s.visibility,
       COALESCE(c.n, 0), s.updated_at, s.match_rate, s.last_upload_at,
       s.taxonomy_id, t.path, s.data_fee_micros
FROM audience_segments s
LEFT JOIN (
    SELECT segment_id, count(*) AS n FROM audience_segment_members GROUP BY segment_id
) c ON c.segment_id = s.id
LEFT JOIN iab_audience_taxonomy t ON t.id = s.taxonomy_id
WHERE s.account_id = $1::uuid
ORDER BY s.updated_at DESC`
		rows, err := tx.QueryContext(ctx, q, accountID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var seg Segment
			if err := rows.Scan(&seg.ID, &seg.Name, &seg.Type, &seg.Status,
				&seg.Source, &seg.Visibility, &seg.Members, &seg.UpdatedAt,
				&seg.MatchRate, &seg.LastUploadAt,
				&seg.TaxonomyID, &seg.TaxonomyPath, &seg.DataFeeMicros); err != nil {
				return err
			}
			out = append(out, seg)
		}
		return rows.Err()
	})
	return out, err
}

// New returns a Store backed by the given *sql.DB. The caller owns the
// connection — Store does not Close it.
func New(db *sql.DB) *Store {
	return &Store{db: db}
}

// SegmentsForUser returns every PUBLIC segment ID the user belongs to.
// Used by the SSP to stamp user.ext.segments on outbound bid requests.
// dsp_private segments are excluded — those belong to a specific DSP and
// must not leak via the bid request to other DSPs.
//
// The query trusts RLS / the calling role for tenant isolation: the SSP
// uses a service role that sees all accounts because the bid-request
// fan-out crosses every advertiser's public segments. Tightening this
// (e.g. per-advertiser views via deals) is a follow-up.
func (s *Store) SegmentsForUser(ctx context.Context, userID string) ([]string, error) {
	return s.queryByVisibility(ctx, userID, "public")
}

// DSPSegmentsForUser returns every dsp_private segment ID the user belongs
// to. Used by the DSP's bid handler to enrich the targeting request with
// its own privately-held audience data (CRM lists, retargeting pixels,
// lookalikes) before campaign matching. Public segments are excluded —
// those already arrived via user.ext.segments on the bid request.
func (s *Store) DSPSegmentsForUser(ctx context.Context, userID string) ([]string, error) {
	return s.queryByVisibility(ctx, userID, "dsp_private")
}

func (s *Store) queryByVisibility(ctx context.Context, userID, visibility string) ([]string, error) {
	if userID == "" {
		return nil, nil
	}
	const q = `
SELECT m.segment_id::text
FROM audience_segment_members m
JOIN audience_segments s ON s.id = m.segment_id
WHERE m.user_id = $1 AND s.visibility = $2`
	var out []string
	// Cross-account read (bid-request fan-out spans every advertiser's
	// segments) → platform hatch so the NOBYPASSRLS app role sees all rows
	// (security #77).
	err := s.withPlatformRead(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, q, userID, visibility)
		if err != nil {
			return fmt.Errorf("query %s segments for user %q: %w", visibility, userID, err)
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return fmt.Errorf("scan segment id: %w", err)
			}
			out = append(out, id)
		}
		return rows.Err()
	})
	return out, err
}

// withPlatformRead runs fn inside a read-only transaction with the platform
// read hatch enabled (app.platform_read='on'), so cross-account service-role
// reads (bid-request fan-out over every tenant's public segments, the SSP
// taxonomy/monetization warm cache) see all rows under the NOBYPASSRLS app
// role. Read-only: the hatch admits reads via the tenant_isolation USING
// clause; it must never wrap a write.
func (s *Store) withPlatformRead(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.platform_read', 'on', true)`); err != nil {
		return fmt.Errorf("set platform read: %w", err)
	}
	return fn(tx)
}

// --- Write path (CRM upload / behavioural rollup / lookalike publish) ---
//
// Writes are account-scoped and RLS-enforced: each runs in a transaction
// that sets app.current_account_id first, matching the tenant isolation the
// read path relies on. In dev the DB role has BYPASSRLS so this is a no-op;
// in prod the production role enforces the policy.

// withTenant runs fn inside a transaction with the RLS tenant GUC set, so
// INSERTs into account-scoped tables are admitted (and can't touch another
// tenant's rows).
func (s *Store) withTenant(ctx context.Context, accountID string, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_account_id', $1, true)`, accountID); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("set tenant: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// UpsertSegment finds-or-creates a segment for (accountID, name) and returns
// its deterministic ID (UUIDv5 over account+name, so re-uploading the same
// named list targets the same segment rather than spawning duplicates).
func (s *Store) UpsertSegment(ctx context.Context, accountID, name, typ, source, visibility string) (string, error) {
	segmentID := idgen.Derive("segment", accountID+"/"+name)
	err := s.withTenant(ctx, accountID, func(tx *sql.Tx) error {
		const q = `
INSERT INTO audience_segments (id, account_id, name, type, status, source, visibility, created_at, updated_at)
VALUES ($1, $2, $3, $4, 'active', $5, $6, now(), now())
ON CONFLICT (id) DO UPDATE SET
    name = EXCLUDED.name, type = EXCLUDED.type, source = EXCLUDED.source,
    visibility = EXCLUDED.visibility, updated_at = now()`
		_, err := tx.ExecContext(ctx, q, segmentID, accountID, name, typ, source, visibility)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("upsert segment %q: %w", name, err)
	}
	return segmentID, nil
}

// RemoveMembersNotIn deletes the segment's members whose user_id is NOT in
// keep — the replace-by-segment prune for rule-derived (behavioural)
// segments, where each profile-builder run recomputes the full member set
// and users who no longer qualify must drop out. Returns rows removed.
func (s *Store) RemoveMembersNotIn(ctx context.Context, accountID, segmentID string, keep []string) (int, error) {
	if keep == nil {
		// pq.Array(nil) encodes SQL NULL, and `NOT (user_id = ANY(NULL))` is
		// NULL — which deletes NOTHING. A nil keep must mean "prune everyone",
		// so force the empty array.
		keep = []string{}
	}
	removed := 0
	err := s.withTenant(ctx, accountID, func(tx *sql.Tx) error {
		const q = `
DELETE FROM audience_segment_members
WHERE segment_id = $1 AND account_id = $2::uuid AND NOT (user_id = ANY($3))`
		res, err := tx.ExecContext(ctx, q, segmentID, accountID, pq.Array(keep))
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		removed = int(n)
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("prune members of %s: %w", segmentID, err)
	}
	return removed, nil
}

// SetSegmentUploadStats persists the match rate of an upload on its segment:
// matched/uploaded, the fraction of uploaded ids resolvable via
// identity_graph. Overwritten per upload (latest upload wins) — the number is
// upload feedback, not a lifetime aggregate.
func (s *Store) SetSegmentUploadStats(ctx context.Context, accountID, segmentID string, uploaded, matched int) error {
	if uploaded <= 0 {
		return nil
	}
	err := s.withTenant(ctx, accountID, func(tx *sql.Tx) error {
		const q = `
UPDATE audience_segments
SET match_rate = $3::float / $4::float, last_upload_at = now(), updated_at = now()
WHERE id = $1 AND account_id = $2::uuid`
		_, err := tx.ExecContext(ctx, q, segmentID, accountID, matched, uploaded)
		return err
	})
	if err != nil {
		return fmt.Errorf("set upload stats on %s: %w", segmentID, err)
	}
	return nil
}

// SetSegmentProvenance stamps the data-provider attribution on a segment (ADR
// 0009): which provider it was ingested from and its resolved data-party
// classification. Both are optional — an empty providerID / dataParty writes SQL
// NULL (a plain first-party upload with no provider). Called by the ingest
// processor after UpsertSegment; the demo/retargeting callers don't set it.
func (s *Store) SetSegmentProvenance(ctx context.Context, accountID, segmentID, providerID, dataParty string) error {
	if providerID == "" && dataParty == "" {
		return nil
	}
	var pid, party any
	if providerID != "" {
		pid = providerID
	}
	if dataParty != "" {
		party = dataParty
	}
	err := s.withTenant(ctx, accountID, func(tx *sql.Tx) error {
		const q = `
UPDATE audience_segments
SET provider_id = $3::uuid, data_party = $4, updated_at = now()
WHERE id = $1 AND account_id = $2::uuid`
		_, err := tx.ExecContext(ctx, q, segmentID, accountID, pid, party)
		return err
	})
	if err != nil {
		return fmt.Errorf("set provenance on %s: %w", segmentID, err)
	}
	return nil
}

// AddMembers bulk-inserts user memberships into a segment (idempotent) and
// returns how many were newly added. The segment must belong to accountID.
func (s *Store) AddMembers(ctx context.Context, accountID, segmentID string, userIDs []string) (int, error) {
	if len(userIDs) == 0 {
		return 0, nil
	}
	added := 0
	err := s.withTenant(ctx, accountID, func(tx *sql.Tx) error {
		const q = `
INSERT INTO audience_segment_members (segment_id, user_id, account_id, added_at)
VALUES ($1, $2, $3, now())
ON CONFLICT (segment_id, user_id) DO NOTHING`
		for _, uid := range userIDs {
			if uid == "" {
				continue
			}
			res, err := tx.ExecContext(ctx, q, segmentID, uid, accountID)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				added++
			}
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("add members to %s: %w", segmentID, err)
	}
	return added, nil
}

// RetargetingSegmentRow is a retargeting segment's id + raw rule JSON, for the
// real-time enroller (cmd/audience-rt) to match against a site visit.
type RetargetingSegmentRow struct {
	ID   string
	Rule []byte
}

// RetargetingSegments returns the account's ACTIVE retargeting segments (id +
// rule JSON). The real-time enroller evaluates each rule against a site_visit.
func (s *Store) RetargetingSegments(ctx context.Context, accountID string) ([]RetargetingSegmentRow, error) {
	out := []RetargetingSegmentRow{}
	err := s.withTenant(ctx, accountID, func(tx *sql.Tx) error {
		const q = `
SELECT id::text, COALESCE(rule, '{}'::jsonb)::text
FROM audience_segments
WHERE account_id = $1::uuid AND type = 'retargeting' AND status = 'active'`
		rows, err := tx.QueryContext(ctx, q, accountID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r RetargetingSegmentRow
			var ruleStr string
			if err := rows.Scan(&r.ID, &ruleStr); err != nil {
				return err
			}
			r.Rule = []byte(ruleStr)
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// RemoveMember deletes one user's membership in a segment — retargeting
// suppression when a shopper converts. Returns rows affected (0 = not a member).
func (s *Store) RemoveMember(ctx context.Context, accountID, segmentID, userID string) (int, error) {
	var removed int
	err := s.withTenant(ctx, accountID, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM audience_segment_members WHERE segment_id = $1 AND user_id = $2 AND account_id = $3::uuid`,
			segmentID, userID, accountID)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		removed = int(n)
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("remove member from %s: %w", segmentID, err)
	}
	return removed, nil
}
