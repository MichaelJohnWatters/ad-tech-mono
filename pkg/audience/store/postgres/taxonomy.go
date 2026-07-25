package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// TaxonomyNode is one row of the global iab_audience_taxonomy reference table
// (migration 062): a node of IAB Audience Taxonomy 1.1. Path is the full
// breadcrumb the portal picker displays and searches over.
type TaxonomyNode struct {
	ID       int64  `json:"id"`
	ParentID *int64 `json:"parent_id,omitempty"`
	Name     string `json:"name"`
	Path     string `json:"path"`
}

// ListTaxonomy returns every taxonomy node ordered by path. Global reference
// data — deliberately NOT tenant-scoped (config_schema precedent): the
// taxonomy is the same for every account, and the picker fuzzy-filters
// client-side over the full list (a few hundred rows at most).
func (s *Store) ListTaxonomy(ctx context.Context) ([]TaxonomyNode, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, parent_id, name, path FROM iab_audience_taxonomy ORDER BY path`)
	if err != nil {
		return nil, fmt.Errorf("list taxonomy: %w", err)
	}
	defer rows.Close()
	out := []TaxonomyNode{}
	for rows.Next() {
		var n TaxonomyNode
		if err := rows.Scan(&n.ID, &n.ParentID, &n.Name, &n.Path); err != nil {
			return nil, fmt.Errorf("scan taxonomy node: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// SetSegmentTaxonomy sets (or, with nil, clears) a segment's IAB Audience
// Taxonomy label. Tenant-scoped: the segment must belong to accountID, so a
// tenant can never label — and thereby externally expose — another tenant's
// segment. An unknown taxonomyID fails on the FK.
func (s *Store) SetSegmentTaxonomy(ctx context.Context, accountID, segmentID string, taxonomyID *int64) error {
	var tid any
	if taxonomyID != nil {
		tid = *taxonomyID
	}
	err := s.withTenant(ctx, accountID, func(tx *sql.Tx) error {
		const q = `
UPDATE audience_segments
SET taxonomy_id = $3, updated_at = now()
WHERE id = $1 AND account_id = $2::uuid`
		res, err := tx.ExecContext(ctx, q, segmentID, accountID, tid)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("segment %s not found for account", segmentID)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("set taxonomy on %s: %w", segmentID, err)
	}
	return nil
}

// SegmentMonetization is the SSP hot-path view of one PUBLIC,
// taxonomy-labelled segment: what rides user.data (the taxonomy node) plus
// what monetizes it (owner + optional data fee, migration 063). FeeMicros is
// a CPM in micro-dollars; 0 = the segment rides but earns nothing.
type SegmentMonetization struct {
	TaxonomyID     int64
	OwnerAccountID string
	FeeMicros      int64
}

// DataEarningsRow is one segment's accrued data-fee earnings for the owning
// account — the portal's "Data earnings" surface. Amounts are summed
// per-impression micro-dollars.
type DataEarningsRow struct {
	SegmentID      string     `json:"segment_id"`
	SegmentName    string     `json:"segment_name"`
	Impressions    int64      `json:"impressions"`
	FeeMicros      int64      `json:"fee_micros"`
	OwnerNetMicros int64      `json:"owner_net_micros"`
	MarginMicros   int64      `json:"margin_micros"`
	LastEarnedAt   *time.Time `json:"last_earned_at,omitempty"`
}

// DataEarnings returns the account's accrued data-fee earnings grouped by
// segment, biggest earner first. Tenant-scoped (RLS + explicit filter).
func (s *Store) DataEarnings(ctx context.Context, accountID string) ([]DataEarningsRow, error) {
	out := []DataEarningsRow{}
	err := s.withTenant(ctx, accountID, func(tx *sql.Tx) error {
		const q = `
SELECT e.segment_id, COALESCE(s.name, e.segment_id), count(*),
       SUM(e.fee_micros), SUM(e.owner_net_micros), SUM(e.margin_micros),
       MAX(e.created_at)
FROM data_fee_earnings e
LEFT JOIN audience_segments s ON s.id::text = e.segment_id
WHERE e.account_id = $1::uuid
GROUP BY e.segment_id, s.name
ORDER BY SUM(e.owner_net_micros) DESC`
		rows, err := tx.QueryContext(ctx, q, accountID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r DataEarningsRow
			if err := rows.Scan(&r.SegmentID, &r.SegmentName, &r.Impressions,
				&r.FeeMicros, &r.OwnerNetMicros, &r.MarginMicros, &r.LastEarnedAt); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("data earnings for %s: %w", accountID, err)
	}
	return out, nil
}

// PublicSegmentMonetization returns segment_id → monetization for every
// PUBLIC segment carrying a taxonomy label, across all accounts. This is the
// SSP's warm-cache source for BOTH the user.data stamp (ext.segtax) and the
// post-auction DataFeeEvent (segment owner + fee). Only labelled segments
// appear: an unlabelled fee-bearing segment never rides user.data, so it
// never earns ("label it to sell it"). Same cross-account service role
// rationale as SegmentsForUser.
func (s *Store) PublicSegmentMonetization(ctx context.Context) (map[string]SegmentMonetization, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id::text, taxonomy_id, account_id::text, COALESCE(data_fee_micros, 0)
FROM audience_segments
WHERE visibility = 'public' AND taxonomy_id IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("public segment monetization: %w", err)
	}
	defer rows.Close()
	out := map[string]SegmentMonetization{}
	for rows.Next() {
		var id string
		var m SegmentMonetization
		if err := rows.Scan(&id, &m.TaxonomyID, &m.OwnerAccountID, &m.FeeMicros); err != nil {
			return nil, fmt.Errorf("scan segment monetization: %w", err)
		}
		out[id] = m
	}
	return out, rows.Err()
}

// SetSegmentDataFee sets (or, with nil, clears) a segment's data fee — the
// CPM in micro-dollars the owning account earns when this PUBLIC, labelled
// segment rides a bid request an external buyer wins. Tenant-scoped like
// SetSegmentTaxonomy.
func (s *Store) SetSegmentDataFee(ctx context.Context, accountID, segmentID string, feeMicros *int64) error {
	if feeMicros != nil && *feeMicros < 0 {
		return fmt.Errorf("data fee must be >= 0, got %d", *feeMicros)
	}
	var fee any
	if feeMicros != nil {
		fee = *feeMicros
	}
	err := s.withTenant(ctx, accountID, func(tx *sql.Tx) error {
		const q = `
UPDATE audience_segments
SET data_fee_micros = $3, updated_at = now()
WHERE id = $1 AND account_id = $2::uuid`
		res, err := tx.ExecContext(ctx, q, segmentID, accountID, fee)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("segment %s not found for account", segmentID)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("set data fee on %s: %w", segmentID, err)
	}
	return nil
}
