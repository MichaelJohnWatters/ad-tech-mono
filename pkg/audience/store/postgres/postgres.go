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
)

// Store reads audience segment memberships from Postgres.
type Store struct {
	db *sql.DB
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
