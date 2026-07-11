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
}

// ListSegments returns every segment for an account with its member count,
// most-recently-updated first. Tenant-scoped via RLS (withTenant sets
// app.current_account_id) plus an explicit account_id filter.
func (s *Store) ListSegments(ctx context.Context, accountID string) ([]Segment, error) {
	out := []Segment{}
	err := s.withTenant(ctx, accountID, func(tx *sql.Tx) error {
		const q = `
SELECT s.id::text, s.name, s.type, s.status, s.source, s.visibility,
       COALESCE(c.n, 0), s.updated_at
FROM audience_segments s
LEFT JOIN (
    SELECT segment_id, count(*) AS n FROM audience_segment_members GROUP BY segment_id
) c ON c.segment_id = s.id
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
				&seg.Source, &seg.Visibility, &seg.Members, &seg.UpdatedAt); err != nil {
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
	rows, err := s.db.QueryContext(ctx, q, userID, visibility)
	if err != nil {
		return nil, fmt.Errorf("query %s segments for user %q: %w", visibility, userID, err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan segment id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
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
