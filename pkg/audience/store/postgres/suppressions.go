package postgres

// suppressions.go — the retargeting purchase burn-list (migration 082).
// Written by audience-rt on purchase (person+household expanded), consulted
// by audience-rt's real-time enrollment (newer-visit-clears semantics) and
// by the profile-builder's site_visit rule pass (so an hourly recompute
// cannot re-qualify a buyer from pre-purchase signals).

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/lib/pq"
)

// Suppress upserts WHOLE-PERSON burn-list rows (sku=”) for every id a purchase
// expands to. Same suppressed_at for the batch (one purchase = one moment); a
// repeat purchase refreshes both timestamps. Per-product burns (a specific sku)
// are written by SuppressSKUs (DPA slice 4).
func (s *Store) Suppress(ctx context.Context, accountID string, userIDs []string, at time.Time, ttl time.Duration, originTrace string) error {
	if len(userIDs) == 0 {
		return nil
	}
	return s.withTenant(ctx, accountID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
INSERT INTO retargeting_suppressions (account_id, user_id, sku, suppressed_at, expires_at, source, origin_trace)
SELECT $1::uuid, unnest($2::text[]), '', $3, $4, 'purchase', $5
ON CONFLICT (account_id, user_id, sku)
DO UPDATE SET suppressed_at = EXCLUDED.suppressed_at, expires_at = EXCLUDED.expires_at, origin_trace = EXCLUDED.origin_trace`,
			accountID, pq.StringArray(userIDs), at, at.Add(ttl), originTrace)
		if err != nil {
			return fmt.Errorf("suppress: %w", err)
		}
		return nil
	})
}

// SuppressSKUs writes PER-PRODUCT burn rows: one row per (id, sku) the purchase
// bought, for every expanded id (person + household). These stop the dynamic
// creative from re-featuring a bought SKU (RecordProductViews filters them) even
// if a stale pixel re-fires — the durable half of per-product suppression (DPA
// slice 4), mirroring the whole-person burn's durability lesson (migration 082).
func (s *Store) SuppressSKUs(ctx context.Context, accountID string, userIDs, skus []string, at time.Time, ttl time.Duration, originTrace string) error {
	if len(userIDs) == 0 || len(skus) == 0 {
		return nil
	}
	return s.withTenant(ctx, accountID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
INSERT INTO retargeting_suppressions (account_id, user_id, sku, suppressed_at, expires_at, source, origin_trace)
SELECT $1::uuid, u.id, k.sku, $4, $5, 'purchase', $6
FROM unnest($2::text[]) AS u(id), unnest($3::text[]) AS k(sku)
ON CONFLICT (account_id, user_id, sku)
DO UPDATE SET suppressed_at = EXCLUDED.suppressed_at, expires_at = EXCLUDED.expires_at, origin_trace = EXCLUDED.origin_trace`,
			accountID, pq.StringArray(userIDs), pq.StringArray(skus), at, at.Add(ttl), originTrace)
		if err != nil {
			return fmt.Errorf("suppress skus: %w", err)
		}
		return nil
	})
}

// SuppressedAt returns when (account, user) was WHOLE-PERSON burn-listed (sku=”),
// if the entry is still live. Expired rows are invisible (read-side filter, same
// idiom as member TTLs) even before the physical purge sweeps them.
func (s *Store) SuppressedAt(ctx context.Context, accountID, userID string) (time.Time, bool, error) {
	var at time.Time
	err := s.withTenant(ctx, accountID, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
SELECT suppressed_at FROM retargeting_suppressions
WHERE account_id = $1::uuid AND user_id = $2 AND sku = '' AND expires_at > now()`,
			accountID, userID).Scan(&at)
	})
	if err == sql.ErrNoRows {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("suppressed_at: %w", err)
	}
	return at, true, nil
}

// ClearSuppression removes the WHOLE-PERSON burn-list entry (sku=”) — a
// genuinely new visit after the purchase reopens the chase. Per-product burns
// are left in place (a new visit to a DIFFERENT product shouldn't un-suppress a
// bought one); they age out on their own TTL.
func (s *Store) ClearSuppression(ctx context.Context, accountID, userID string) error {
	return s.withTenant(ctx, accountID, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
DELETE FROM retargeting_suppressions WHERE account_id = $1::uuid AND user_id = $2 AND sku = ''`,
			accountID, userID); err != nil {
			return fmt.Errorf("clear suppression: %w", err)
		}
		return nil
	})
}

// PurgeExpiredSuppressions physically deletes burn-list rows past their TTL —
// cross-tenant storage hygiene under the platform hatch, exactly like
// PurgeExpiredMembers (the read path already excludes expired rows).
func (s *Store) PurgeExpiredSuppressions(ctx context.Context) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin purge suppressions: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.platform_read', 'on', true)`); err != nil {
		return 0, fmt.Errorf("purge suppressions platform-read: %w", err)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM retargeting_suppressions WHERE expires_at <= now()`)
	if err != nil {
		return 0, fmt.Errorf("purge expired suppressions: %w", err)
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit purge suppressions: %w", err)
	}
	return int(n), nil
}

// ExpandPerson returns the additional ids a converting user's purchase
// suppresses: identity-cluster siblings (the person's other devices, from
// the profile-builder's serving copy) and linked household ids from the
// identity graph. Both tables are platform-global (no RLS). Read-only.
func (s *Store) ExpandPerson(ctx context.Context, userID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT sibling.member_id
FROM identity_clusters me
JOIN identity_clusters sibling ON sibling.person_id = me.person_id
WHERE me.member_id = $1 AND sibling.member_id <> $1
UNION
SELECT linked_id FROM identity_graph WHERE user_id = $1 AND linked_id LIKE 'hh:%'
UNION
SELECT user_id FROM identity_graph WHERE linked_id = $1 AND user_id LIKE 'hh:%'`,
		userID)
	if err != nil {
		return nil, fmt.Errorf("expand person: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
