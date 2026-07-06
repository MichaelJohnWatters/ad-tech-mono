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

// Save upserts today's settled cents for each campaign under the given UTC day.
// Whole map in one transaction so a mid-write crash leaves a consistent row set.
// An empty map is a no-op (nothing billed yet).
func (s *CommittedSpendStore) Save(ctx context.Context, day string, cents map[string]int64) error {
	if s.Store == nil {
		return sql.ErrConnDone
	}
	if len(cents) == 0 {
		return nil
	}
	tx, err := s.Store.primary.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin committed-spend save: %w", err)
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO campaign_committed_spend (day, campaign_id, settled_cents, updated_at)
VALUES ($1, $2, $3, now())
ON CONFLICT (day, campaign_id) DO UPDATE SET
    settled_cents = EXCLUDED.settled_cents, updated_at = now()`)
	if err != nil {
		return fmt.Errorf("prepare committed-spend save: %w", err)
	}
	defer stmt.Close()
	for campaignID, c := range cents {
		if _, err := stmt.ExecContext(ctx, day, campaignID, c); err != nil {
			return fmt.Errorf("upsert committed-spend %s: %w", campaignID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit committed-spend save: %w", err)
	}
	return nil
}

// Load returns the persisted settled cents per campaign for the given UTC day.
// Empty map when there's no row (fresh day / first boot).
func (s *CommittedSpendStore) Load(ctx context.Context, day string) (map[string]int64, error) {
	out := make(map[string]int64)
	if s.Store == nil {
		return out, sql.ErrConnDone
	}
	rows, err := s.Store.read.QueryContext(ctx,
		`SELECT campaign_id, settled_cents FROM campaign_committed_spend WHERE day = $1`, day)
	if err != nil {
		return out, fmt.Errorf("load committed-spend: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var cents int64
		if err := rows.Scan(&id, &cents); err != nil {
			return out, fmt.Errorf("scan committed-spend: %w", err)
		}
		out[id] = cents
	}
	return out, rows.Err()
}
