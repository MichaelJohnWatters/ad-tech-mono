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
WHERE m.user_id = $1 AND s.visibility = $2
  AND (m.expires_at IS NULL OR m.expires_at > now())`
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
// source / originTrace are the lineage stamp (migration 080): which writer
// enrolled the user and under what correlation id (an "ing_…" ingest job, a
// 32-hex request trace, a "batch_…" conductor run — self-typed by format).
// ON CONFLICT DO NOTHING makes lineage FIRST-WRITER-WINS: a re-run or a second
// writer re-adding an existing member never overwrites the original origin.
func (s *Store) AddMembers(ctx context.Context, accountID, segmentID string, userIDs []string, source, originTrace string) (int, error) {
	if len(userIDs) == 0 {
		return 0, nil
	}
	added := 0
	err := s.withTenant(ctx, accountID, func(tx *sql.Tx) error {
		const q = `
INSERT INTO audience_segment_members (segment_id, user_id, account_id, added_at, source, origin_trace)
VALUES ($1, $2, $3, now(), $4, $5)
ON CONFLICT (segment_id, user_id) DO NOTHING`
		for _, uid := range userIDs {
			if uid == "" {
				continue
			}
			res, err := tx.ExecContext(ctx, q, segmentID, uid, accountID, source, originTrace)
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

// AddMembersWithExpiry is AddMembers with a TTL: members enrolled with a
// non-nil expiresAt age out (read paths exclude expired rows). Used by the
// real-time retargeting enroller so an abandoner who never converts stops being
// retargeted at expires_at. On CONFLICT it refreshes expires_at (a repeat visit
// extends the window).
//
// Returns the count of NEWLY-INSERTED members only (not window refreshes) — via
// the `xmax = 0` idiom, true iff the row was inserted rather than updated. This
// is what makes real-time retargeting FIRST-ENROLL-only: the enroller invalidates
// the audience cache and fires the enrolled webhook only when someone genuinely
// enters the pool, not on every repeat visit (which would flood the preloader
// reload + the webhook). A returning member's window is still silently extended.
// source / originTrace stamp lineage as in AddMembers; the conflict arm only
// refreshes expires_at, so a repeat visit extends the window WITHOUT
// overwriting the first enrolment's origin (first-writer-wins).
func (s *Store) AddMembersWithExpiry(ctx context.Context, accountID, segmentID string, userIDs []string, expiresAt *time.Time, source, originTrace string) (int, error) {
	if len(userIDs) == 0 {
		return 0, nil
	}
	added := 0
	err := s.withTenant(ctx, accountID, func(tx *sql.Tx) error {
		const q = `
INSERT INTO audience_segment_members (segment_id, user_id, account_id, added_at, expires_at, source, origin_trace)
VALUES ($1, $2, $3, now(), $4, $5, $6)
ON CONFLICT (segment_id, user_id) DO UPDATE SET expires_at = EXCLUDED.expires_at
RETURNING (xmax = 0)`
		for _, uid := range userIDs {
			if uid == "" {
				continue
			}
			var inserted bool
			if err := tx.QueryRowContext(ctx, q, segmentID, uid, accountID, expiresAt, source, originTrace).Scan(&inserted); err != nil {
				return err
			}
			if inserted {
				added++
			}
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("add members (ttl) to %s: %w", segmentID, err)
	}
	return added, nil
}

// PurgeExpiredMembers physically deletes retargeting members past their TTL. The
// read paths already exclude expired rows (expires_at <= now()), so this is pure
// storage hygiene. It's cross-tenant, so it runs under the platform hatch — the
// audience_segment_members tenant_isolation policy is USING-only, so
// app.platform_read='on' admits the delete (same pattern as the data-fee settle).
// Returns the number of rows deleted.
func (s *Store) PurgeExpiredMembers(ctx context.Context) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin purge: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.platform_read', 'on', true)`); err != nil {
		return 0, fmt.Errorf("purge platform-read: %w", err)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM audience_segment_members WHERE expires_at IS NOT NULL AND expires_at <= now()`)
	if err != nil {
		return 0, fmt.Errorf("purge expired members: %w", err)
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit purge: %w", err)
	}
	return int(n), nil
}

// MembershipRow is one live (non-expired) membership: which user is in which
// segment, at what visibility. Used by the audience cache writer's reconcile to
// rebuild the Redis sets from Postgres truth.
type MembershipRow struct {
	UserID     string
	Visibility string
	SegmentID  string
}

// AllMemberships returns every live membership across all accounts (platform-read
// hatch), for the single cache writer's full-scan reconcile.
func (s *Store) AllMemberships(ctx context.Context) ([]MembershipRow, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin memberships scan: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.platform_read', 'on', true)`); err != nil {
		return nil, fmt.Errorf("memberships scan platform-read: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `
SELECT m.user_id, s.visibility, m.segment_id::text
FROM audience_segment_members m
JOIN audience_segments s ON s.id = m.segment_id
WHERE m.expires_at IS NULL OR m.expires_at > now()`)
	if err != nil {
		return nil, fmt.Errorf("query memberships: %w", err)
	}
	defer rows.Close()
	var out []MembershipRow
	for rows.Next() {
		var r MembershipRow
		if err := rows.Scan(&r.UserID, &r.Visibility, &r.SegmentID); err != nil {
			return nil, fmt.Errorf("scan membership: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SegmentOverlap holds the live-member set sizes of two segments and their
// intersection — the raw inputs to a marketplace expansion estimate.
type SegmentOverlap struct {
	SizeA   int // live members of segment A (the buyer's own audience)
	SizeB   int // live members of segment B (the listing's segment)
	Overlap int // members in BOTH (|A ∩ B|)
}

// EstimateOverlap counts each segment's live members and their intersection —
// a real set operation on the two member sets (expired members excluded, same
// as the serving read paths). Cross-tenant under the platform hatch: the buyer's
// segment and the seller's listed segment belong to different accounts, and the
// caller (marketplace estimate) is a platform read. Returns ONLY aggregate
// counts — never a member list. The privacy min-aggregation floor is applied by
// the caller (marketplace handler), not here.
func (s *Store) EstimateOverlap(ctx context.Context, segA, segB string) (SegmentOverlap, error) {
	var o SegmentOverlap
	err := s.withPlatformRead(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
SELECT
  (SELECT count(*) FROM audience_segment_members
     WHERE segment_id = $1::uuid AND (expires_at IS NULL OR expires_at > now())),
  (SELECT count(*) FROM audience_segment_members
     WHERE segment_id = $2::uuid AND (expires_at IS NULL OR expires_at > now())),
  (SELECT count(*) FROM audience_segment_members a
     WHERE a.segment_id = $1::uuid AND (a.expires_at IS NULL OR a.expires_at > now())
       AND EXISTS (SELECT 1 FROM audience_segment_members b
                   WHERE b.segment_id = $2::uuid AND b.user_id = a.user_id
                     AND (b.expires_at IS NULL OR b.expires_at > now())))`,
			segA, segB).Scan(&o.SizeA, &o.SizeB, &o.Overlap)
	})
	if err != nil {
		return SegmentOverlap{}, fmt.Errorf("estimate overlap: %w", err)
	}
	return o, nil
}

// MembershipChange is one appended row of audience_membership_changelog — the
// outbox that drives the audience cache's append-based refresh.
type MembershipChange struct {
	Seq        int64
	AccountID  string
	UserID     string
	SegmentID  string
	Visibility string // public | dsp_private
	Op         string // add | remove
}

// AppendMembershipChanges appends outbox rows for one account's membership change
// (retargeting enroll/suppress, upload, profile-builder add/prune), under the
// account's tenant context so RLS admits the insert. A best-effort companion to
// the members write; a dropped append is caught by the reconcile.
func (s *Store) AppendMembershipChanges(ctx context.Context, accountID string, changes []MembershipChange) error {
	if len(changes) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin changelog append: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_account_id', $1, true)`, accountID); err != nil {
		return fmt.Errorf("changelog append account-scope: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO audience_membership_changelog (account_id, user_id, segment_id, visibility, op)
VALUES ($1::uuid, $2, $3::uuid, $4, $5)`)
	if err != nil {
		return fmt.Errorf("prepare changelog append: %w", err)
	}
	defer stmt.Close()
	for _, c := range changes {
		if _, err := stmt.ExecContext(ctx, accountID, c.UserID, c.SegmentID, c.Visibility, c.Op); err != nil {
			return fmt.Errorf("append changelog row: %w", err)
		}
	}
	return tx.Commit()
}

// ReadMembershipChangesSince returns up to limit changelog rows past afterSeq, in
// seq order, across all accounts (platform-read hatch — the single cache writer
// applies every tenant's changes to the one shared Redis).
func (s *Store) ReadMembershipChangesSince(ctx context.Context, afterSeq int64, limit int) ([]MembershipChange, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin changelog read: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.platform_read', 'on', true)`); err != nil {
		return nil, fmt.Errorf("changelog read platform-read: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `
SELECT seq, account_id::text, user_id, segment_id::text, visibility, op
FROM audience_membership_changelog
WHERE seq > $1 ORDER BY seq ASC LIMIT $2`, afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("query changelog: %w", err)
	}
	defer rows.Close()
	var out []MembershipChange
	for rows.Next() {
		var c MembershipChange
		if err := rows.Scan(&c.Seq, &c.AccountID, &c.UserID, &c.SegmentID, &c.Visibility, &c.Op); err != nil {
			return nil, fmt.Errorf("scan changelog row: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ChangelogBacklog reports how far behind the single cache writer is: the number
// of un-drained change-log rows and the age (seconds) of the oldest one. Rows are
// trimmed after they're applied, so a growing backlog / rising oldest-age means
// the writer can't keep up — the signal that it's time to shard the writer.
func (s *Store) ChangelogBacklog(ctx context.Context) (count int, oldestAgeSec float64, err error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return 0, 0, fmt.Errorf("begin backlog: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.platform_read', 'on', true)`); err != nil {
		return 0, 0, fmt.Errorf("backlog platform-read: %w", err)
	}
	err = tx.QueryRowContext(ctx, `
SELECT count(*), COALESCE(EXTRACT(EPOCH FROM (now() - min(changed_at))), 0)
FROM audience_membership_changelog`).Scan(&count, &oldestAgeSec)
	if err != nil {
		return 0, 0, fmt.Errorf("query backlog: %w", err)
	}
	return count, oldestAgeSec, nil
}

// TrimMembershipChanges deletes consumed changelog rows (seq <= uptoSeq) across
// all accounts, so the outbox stays bounded. Called by the single writer after
// it has applied + advanced its watermark.
func (s *Store) TrimMembershipChanges(ctx context.Context, uptoSeq int64) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin changelog trim: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.platform_read', 'on', true)`); err != nil {
		return 0, fmt.Errorf("changelog trim platform-read: %w", err)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM audience_membership_changelog WHERE seq <= $1`, uptoSeq)
	if err != nil {
		return 0, fmt.Errorf("trim changelog: %w", err)
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit changelog trim: %w", err)
	}
	return int(n), nil
}

// RetargetingSegmentRow is a retargeting segment's id + raw rule JSON, for the
// real-time enroller (cmd/audience-rt) to match against a site visit.
type RetargetingSegmentRow struct {
	ID         string
	Rule       []byte
	Visibility string
}

// RetargetingSegments returns the account's ACTIVE retargeting segments (id +
// rule JSON). The real-time enroller evaluates each rule against a site_visit.
func (s *Store) RetargetingSegments(ctx context.Context, accountID string) ([]RetargetingSegmentRow, error) {
	out := []RetargetingSegmentRow{}
	err := s.withTenant(ctx, accountID, func(tx *sql.Tx) error {
		const q = `
SELECT id::text, COALESCE(rule, '{}'::jsonb)::text, visibility
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
			if err := rows.Scan(&r.ID, &ruleStr, &r.Visibility); err != nil {
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

// RetargetingSegmentUI is a retargeting segment for the advertiser portal: its
// rule (pixel tag + TTL window) plus the LIVE enrollment count (non-expired
// members).
type RetargetingSegmentUI struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Tag        string    `json:"tag"`
	WindowDays int       `json:"window_days"`
	Members    int       `json:"members"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// ListRetargetingSegments returns the account's retargeting segments with their
// tag/window and current (non-expired) enrollment count, for the portal.
func (s *Store) ListRetargetingSegments(ctx context.Context, accountID string) ([]RetargetingSegmentUI, error) {
	out := []RetargetingSegmentUI{}
	err := s.withTenant(ctx, accountID, func(tx *sql.Tx) error {
		const q = `
SELECT s.id::text, s.name, COALESCE(s.rule->>'tag', ''),
       COALESCE((s.rule->>'window_days')::int, 30), COALESCE(c.n, 0), s.updated_at
FROM audience_segments s
LEFT JOIN (
    SELECT segment_id, count(*) AS n FROM audience_segment_members
    WHERE expires_at IS NULL OR expires_at > now()
    GROUP BY segment_id
) c ON c.segment_id = s.id
WHERE s.account_id = $1::uuid AND s.type = 'retargeting'
ORDER BY s.updated_at DESC`
		rows, err := tx.QueryContext(ctx, q, accountID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r RetargetingSegmentUI
			if err := rows.Scan(&r.ID, &r.Name, &r.Tag, &r.WindowDays, &r.Members, &r.UpdatedAt); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// CreateRetargetingSegment creates a single-visit retargeting segment (the target
// of the /v1/t/rt pixel): type retargeting, dsp_private, rule {site_visit, tag,
// min_count 1, window_days}. cmd/audience-rt enrolls visitors into it in real time.
func (s *Store) CreateRetargetingSegment(ctx context.Context, accountID, name, tag string, windowDays int) (string, error) {
	if windowDays <= 0 {
		windowDays = 30
	}
	var id string
	err := s.withTenant(ctx, accountID, func(tx *sql.Tx) error {
		const q = `
INSERT INTO audience_segments (account_id, name, type, status, source, visibility, rule)
VALUES ($1::uuid, $2, 'retargeting', 'active', 'portal', 'dsp_private',
        jsonb_build_object('event', 'site_visit', 'tag', $3::text, 'min_count', 1, 'window_days', $4::int))
RETURNING id::text`
		return tx.QueryRowContext(ctx, q, accountID, name, tag, windowDays).Scan(&id)
	})
	if err != nil {
		return "", fmt.Errorf("create retargeting segment: %w", err)
	}
	return id, nil
}
