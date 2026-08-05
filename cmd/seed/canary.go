package main

// Overspend canary: one advertiser funded with a DELIBERATELY tiny balance
// ($5) behind a competitive, broadly-targeted campaign with a huge daily
// budget — so under any load run the BALANCE gate (not the budget pacer) is
// the binding constraint, and it binds within the first minute of traffic.
//
// Purpose: the overspend watchdog (adtech_billing_overspend_usd_total) would
// otherwise read a permanent, untested zero — the well-funded demo world
// never exercises depletion. The canary makes every load run a live test of
// the gate: it depletes fast, the gate closes, and whatever small negative
// balance remains IS the measured gate precision (design bound: seconds of
// win volume ≈ cents; a dollars-deep negative here means the gate's
// staleness budget regressed). Post-run check:
//
//	SELECT balance FROM advertiser_balances
//	 WHERE account_id = DeriveID("account","canary-lowbal")
//
// ≈ 0, slightly negative allowed.

import (
	"context"
	"fmt"
)

const canaryAccountKey = "canary-lowbal"

// canaryGrantUSD is small enough to deplete in under a minute of load
// (~$0.20-0.30/s of win volume at 100rps when it's winning its share) but big
// enough to serve a visible burst first.
const canaryGrantUSD = 5.00

// SeedOverspendCanary creates the canary advertiser + campaign + tiny grant.
// Idempotent like every other seed path.
func (in *inserter) SeedOverspendCanary(ctx context.Context) error {
	internalDSP := DeriveID("dsp", "internal")
	if err := in.upsertAccounts(ctx,
		map[string]string{canaryAccountKey: "Canary Low-Balance (overspend probe)"},
		map[string]string{canaryAccountKey: internalDSP}); err != nil {
		return fmt.Errorf("canary account: %w", err)
	}
	ioKey := canaryAccountKey + "-io"
	if err := in.upsertInsertionOrders(ctx, map[string]ioInsertPayload{
		ioKey: {external: ioKey, accountID: DeriveID("account", canaryAccountKey), name: "Canary IO", currency: "USD"},
	}); err != nil {
		return fmt.Errorf("canary IO: %w", err)
	}
	// Bid placement is deliberate: 7.5 flat, RON. Deterministic internal
	// bids mean strict effective-price ordering — at 6.5/USA-only the canary
	// NEVER won (Acme US ≈10.9 effective with modifiers, Initech GBR ≈7.3)
	// and sat at $5 forever, defeating its purpose. 7.5 RON tops the non-US
	// display slice (beats Initech's ~7.3 there) while staying under Acme's
	// modified home-market bids, so it wins a steady minority share and the
	// $5 drains within ~a minute of load. DailyBudget huge so the BUDGET
	// gate never binds before the BALANCE gate — exercising the latter is
	// the whole point.
	cc := CampaignConfig{
		ID:          canaryAccountKey + "-c0",
		AccountID:   canaryAccountKey,
		IOId:        ioKey,
		Name:        "Canary - Overspend Probe (display RON)",
		BaseBid:     7.5,
		Currency:    "USD",
		DailyBudget: 100000,
		BidModel:    "cpm",
		PacingMode:  "asap",
		Status:      "live",
		Format:      "display",
		Creatives: []CreativeYAML{
			{Width: 300, Height: 250}, {Width: 728, Height: 90},
			{Width: 320, Height: 50}, {Width: 300, Height: 600},
		},
		Targeting: &TargetingYAML{},
	}
	if err := in.upsertCampaign(ctx, cc); err != nil {
		return fmt.Errorf("canary campaign: %w", err)
	}
	return in.grantBalance(ctx, DeriveID("account", canaryAccountKey), canaryGrantUSD, "seed-canary-grant")
}
