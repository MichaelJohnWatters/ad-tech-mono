package postgres

// product_views.go — SKU-aware retargeting memory (migration 084, DPA slice 2).
// Written by audience-rt when a /v1/t/rt pixel carries SKUs (person+household
// expanded, like suppression). Read at render time (slice 3) to assemble a
// dynamic creative from the user's carted products, and keyed per-SKU for
// cross-sell / per-product suppression (slice 4).

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/lib/pq"
)

// RecordProductViews upserts one row per (account, user, sku), refreshing
// seen_at + expires_at on a repeat view. All SKUs in a single pixel share one
// seen_at (one visit = one moment).
func (s *Store) RecordProductViews(ctx context.Context, accountID, userID string, skus []string, at time.Time, ttl time.Duration, originTrace string) error {
	if userID == "" || len(skus) == 0 {
		return nil
	}
	return s.withTenant(ctx, accountID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
INSERT INTO retargeting_product_views (account_id, user_id, sku, seen_at, expires_at, origin_trace)
SELECT $1::uuid, $2, unnest($3::text[]), $4, $5, $6
ON CONFLICT (account_id, user_id, sku)
DO UPDATE SET seen_at = EXCLUDED.seen_at, expires_at = EXCLUDED.expires_at, origin_trace = EXCLUDED.origin_trace`,
			accountID, userID, pq.StringArray(skus), at, at.Add(ttl), originTrace)
		if err != nil {
			return fmt.Errorf("record product views: %w", err)
		}
		return nil
	})
}

// RecentSKUs returns the user's most-recently-viewed SKUs for the advertiser,
// freshest first, excluding expired rows (read-side TTL filter). limit<=0
// defaults to 10. Reads cross-tenant under the platform hatch — the render
// caller (ad server) has no tenant session, only the advertiser id from the
// won bid.
func (s *Store) RecentSKUs(ctx context.Context, accountID, userID string, limit int) ([]string, error) {
	if userID == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin recent skus: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.platform_read', 'on', true)`); err != nil {
		return nil, fmt.Errorf("recent skus platform-read: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `
SELECT sku FROM retargeting_product_views
WHERE account_id = $1::uuid AND user_id = $2 AND expires_at > now()
ORDER BY seen_at DESC LIMIT $3`, accountID, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("recent skus: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var sku string
		if err := rows.Scan(&sku); err != nil {
			return nil, err
		}
		out = append(out, sku)
	}
	return out, rows.Err()
}

// RemoveProductView deletes one (account, user, sku) row — slice 4 uses it to
// stop showing a SKU the user just bought. No-op if the row is absent.
func (s *Store) RemoveProductView(ctx context.Context, accountID, userID, sku string) error {
	return s.withTenant(ctx, accountID, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
DELETE FROM retargeting_product_views WHERE account_id = $1::uuid AND user_id = $2 AND sku = $3`,
			accountID, userID, sku); err != nil {
			return fmt.Errorf("remove product view: %w", err)
		}
		return nil
	})
}

// PurgeExpiredProductViews physically deletes SKU rows past their TTL —
// cross-tenant storage hygiene under the platform hatch, like the member and
// suppression purges (the read path already excludes expired rows).
func (s *Store) PurgeExpiredProductViews(ctx context.Context) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin purge product views: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.platform_read', 'on', true)`); err != nil {
		return 0, fmt.Errorf("purge product views platform-read: %w", err)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM retargeting_product_views WHERE expires_at <= now()`)
	if err != nil {
		return 0, fmt.Errorf("purge expired product views: %w", err)
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit purge product views: %w", err)
	}
	return int(n), nil
}
