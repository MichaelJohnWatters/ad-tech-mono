package postgres

import (
	"context"
	"database/sql"
	"fmt"
)

// CommittedSpendStore persists the billing engine's per-campaign settled-spend
// so a reporting restart can re-hydrate it instead of resetting committed spend
// (and reconciling DSP pacing counters) to zero. Billing-internal: keyed by
// (day, campaign), no tenant scoping. Backs pkg/billing's SettledToday /
// HydrateSettled.
type CommittedSpendStore struct {
	Store *Store
}

// Save upserts today's settled + open-reserved MICRO-dollars for each campaign
// under the
// given UTC day (migration 040 renamed the columns to match — the values were
// always micros post money-precision). Whole set in one transaction so a
// mid-write crash leaves a
// consistent row set. Campaigns present in either map are written (union), so a
// campaign with only reserves (no settled yet) is still persisted.
func (s *CommittedSpendStore) Save(ctx context.Context, day string, settled, reserved map[string]int64) error {
	if s.Store == nil {
		return sql.ErrConnDone
	}
	if len(settled) == 0 && len(reserved) == 0 {
		return nil
	}
	tx, err := s.Store.primary.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin committed-spend save: %w", err)
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO campaign_committed_spend (day, campaign_id, settled_micros, reserved_micros, updated_at)
VALUES ($1, $2, $3, $4, now())
ON CONFLICT (day, campaign_id) DO UPDATE SET
    settled_micros = EXCLUDED.settled_micros, reserved_micros = EXCLUDED.reserved_micros, updated_at = now()`)
	if err != nil {
		return fmt.Errorf("prepare committed-spend save: %w", err)
	}
	defer stmt.Close()
	seen := make(map[string]struct{}, len(settled)+len(reserved))
	for id := range settled {
		seen[id] = struct{}{}
	}
	for id := range reserved {
		seen[id] = struct{}{}
	}
	for id := range seen {
		if _, err := stmt.ExecContext(ctx, day, id, settled[id], reserved[id]); err != nil {
			return fmt.Errorf("upsert committed-spend %s: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit committed-spend save: %w", err)
	}
	return nil
}

// Load returns the persisted settled + open-reserved micro-dollars per campaign
// for the
// given UTC day. Empty maps when there's no row (fresh day / first boot).
func (s *CommittedSpendStore) Load(ctx context.Context, day string) (settled, reserved map[string]int64, err error) {
	settled = make(map[string]int64)
	reserved = make(map[string]int64)
	if s.Store == nil {
		return settled, reserved, sql.ErrConnDone
	}
	rows, err := s.Store.read.QueryContext(ctx,
		`SELECT campaign_id, settled_micros, reserved_micros FROM campaign_committed_spend WHERE day = $1`, day)
	if err != nil {
		return settled, reserved, fmt.Errorf("load committed-spend: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var s, r int64
		if err := rows.Scan(&id, &s, &r); err != nil {
			return settled, reserved, fmt.Errorf("scan committed-spend: %w", err)
		}
		if s > 0 {
			settled[id] = s
		}
		if r > 0 {
			reserved[id] = r
		}
	}
	return settled, reserved, rows.Err()
}
