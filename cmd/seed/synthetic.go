package main

import (
	"context"
	"fmt"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/idgen"
)

// SeedSyntheticDensity is the SECOND tier of the themed two-tier world:
// bulk dummy segments + memberships seeded AROUND the readable core so
// density/cadence benchmarks (audience-pipeline Phase 3) run at production
// scale without burying the human-checkable dog-lovers/cat-lovers story.
//
// Everything is deterministic and additive: segment ids derive from
// "synthetic-NNN", user ids cycle a fixed universe ("synth-user-NNNNNN") so
// users overlap across segments the way real audiences do (a user lands in
// segments*membersPer/users segments on average). Memberships are inserted
// with one multi-row generate_series statement per segment — a Go-side loop
// of 1M single inserts would take hours.
//
// Deliberate consequence: every membership INSERT fires the migration-078
// changelog trigger, so a big synthetic seed enqueues the same volume into
// audience_membership_changelog for the pipeline writer to drain. That
// backlog-then-drain is a MEASUREMENT TARGET of the density run, not an
// accident — but it means a 1M-member seed keeps the drainer busy for a
// while after the seed exits. Watch adtech_audience_cache_changelog_backlog.
func (in *inserter) SeedSyntheticDensity(ctx context.Context, segments, membersPer, users int) error {
	if users < 1 {
		users = 1
	}
	account := idgen.Derive("account", "adv-synthetic")
	if _, err := in.db.ExecContext(ctx, `
INSERT INTO accounts (id, name, email, type, currency, status, created_at, updated_at)
VALUES ($1, 'Synthetic Density Tier', 'adv-synthetic@seed.local', 'advertiser', 'USD', 'active', now(), now())
ON CONFLICT (id) DO UPDATE SET status = 'active', updated_at = now()`, account); err != nil {
		return fmt.Errorf("synthetic account: %w", err)
	}

	start := time.Now()
	total := 0
	for i := 0; i < segments; i++ {
		segID := idgen.Derive("segment", fmt.Sprintf("synthetic-%03d", i))
		if _, err := in.db.ExecContext(ctx, `
INSERT INTO audience_segments (id, account_id, name, type, visibility, size_estimate, status, source, created_at, updated_at)
VALUES ($1, $2, $3, 'first_party', 'dsp_private', $4, 'active', 'seed_synthetic', now(), now())
ON CONFLICT (id) DO UPDATE SET size_estimate = EXCLUDED.size_estimate, status = 'active', updated_at = now()`,
			segID, account, fmt.Sprintf("Synthetic Segment %03d", i), membersPer); err != nil {
			return fmt.Errorf("synthetic segment %d: %w", i, err)
		}
		// Users cycle the universe with a per-segment stride so consecutive
		// segments don't all hold the same user block.
		if _, err := in.db.ExecContext(ctx, `
INSERT INTO audience_segment_members (segment_id, user_id, account_id, added_at)
SELECT $1, 'synth-user-' || lpad((($2::bigint * 7919 + g) % $3)::text, 6, '0'), $4, now()
FROM generate_series(0, $5 - 1) AS g
ON CONFLICT (segment_id, user_id) DO NOTHING`,
			segID, i, users, account, membersPer); err != nil {
			return fmt.Errorf("synthetic members for segment %d: %w", i, err)
		}
		total += membersPer
	}
	in.log.Info("seeded synthetic density tier",
		"segments", segments, "members_per", membersPer, "user_universe", users,
		"memberships_inserted", total, "elapsed", time.Since(start).String(),
		"note", "changelog trigger enqueued the same volume — watch the pipeline drainer backlog")
	return nil
}

// SeedSyntheticCampaigns adds audience-PREDICATE campaigns over the synthetic
// segments — the piece the big world never had (bigworld.go targets
// geo+device only, so auction-side audience work was unmeasured). Each
// campaign includes one synthetic segment and carries an audience bid
// modifier on it, so a density load run exercises the full chain: Redis
// SMEMBERS → targeting include match → ApplyModifiers pricing. Bids stay in
// the standard 1.5-5.9 band so the market composition the perf baselines
// assume is not distorted.
func (in *inserter) SeedSyntheticCampaigns(ctx context.Context, campaigns, segments int) error {
	if segments < 1 {
		return fmt.Errorf("synthetic campaigns need --synthetic-segments > 0")
	}
	const acctKey = "adv-synthetic"
	ioKey := acctKey + "-io"
	internalDSP := DeriveID("dsp", "internal")
	if err := in.upsertAccounts(ctx,
		map[string]string{acctKey: "Synthetic Density Tier"},
		map[string]string{acctKey: internalDSP}); err != nil {
		return fmt.Errorf("synthetic advertiser: %w", err)
	}
	if err := in.upsertInsertionOrders(ctx, map[string]ioInsertPayload{
		ioKey: {external: ioKey, accountID: DeriveID("account", acctKey), name: "Synthetic Density IO", currency: "USD"},
	}); err != nil {
		return fmt.Errorf("synthetic IO: %w", err)
	}
	budgets := []float64{300, 500, 1000}
	for c := 0; c < campaigns; c++ {
		segKey := fmt.Sprintf("synthetic-%03d", c%segments)
		cc := CampaignConfig{
			ID:          fmt.Sprintf("synthetic-camp-%03d", c),
			AccountID:   acctKey,
			IOId:        ioKey,
			Name:        fmt.Sprintf("Synthetic Audience Campaign %03d (%s)", c, segKey),
			BaseBid:     1.5 + float64(c%12)*0.4,
			Currency:    "USD",
			DailyBudget: budgets[c%len(budgets)],
			BidModel:    "cpm",
			PacingMode:  "even",
			Status:      "live",
			Creatives: []CreativeYAML{
				{Width: 300, Height: 250}, {Width: 728, Height: 90}, {Width: 320, Height: 50},
			},
			Targeting: &TargetingYAML{
				Include: TargetingSetYAML{Segments: []string{segKey}},
			},
			Modifiers: &ModifiersYAML{
				Audience: map[string]float64{segKey: 10 + float64(c%3)*10}, // +10/+20/+30
			},
		}
		if err := in.upsertCampaign(ctx, cc); err != nil {
			return fmt.Errorf("synthetic campaign %d: %w", c, err)
		}
	}
	in.log.Info("seeded synthetic audience campaigns", "campaigns", campaigns, "segments_targeted", min(campaigns, segments))
	return nil
}
