package postgres

import (
	"context"
	"database/sql"
	"fmt"
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

// PublicSegmentTaxonomy returns segment_id → taxonomy_id for every PUBLIC
// segment carrying a taxonomy label, across all accounts. This is the SSP's
// warm-cache source for stamping OpenRTB user.data (ext.segtax): the SSP has
// already decided which public segment ids ride the bid request; this map
// just translates them to standard taxonomy ids. Same cross-account service
// role rationale as SegmentsForUser.
func (s *Store) PublicSegmentTaxonomy(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id::text, taxonomy_id
FROM audience_segments
WHERE visibility = 'public' AND taxonomy_id IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("public segment taxonomy: %w", err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var id string
		var tid int64
		if err := rows.Scan(&id, &tid); err != nil {
			return nil, fmt.Errorf("scan segment taxonomy: %w", err)
		}
		out[id] = tid
	}
	return out, rows.Err()
}
