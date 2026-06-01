//go:build e2e

// Billing model tests — verify the reserve/settle pattern works for CPC
// and CPA. Both models reserve on impression and settle on the trigger
// event (click for CPC, conversion for CPA). The reservation is what
// accrues to the ledger.
//
// These tests skip until the reporting service's billing engine is wired
// for non-CPM models in the unified consumer. The pkg/billing engine
// supports reserve/settle, but the cmd/reporting consumer in this repo
// snapshot processes every impression as CPM. Once the consumer dispatches
// per BidModel, flip t.Skip → real assertion.
package e2e

import "testing"

func TestBillingCPCReserveAndSettle(t *testing.T) {
	t.Skip("CPC reserve-settle dispatch in cmd/reporting consumer pending; pkg/billing engine has the path, the wiring doesn't yet")
}

func TestBillingCPAReserveAndSettle(t *testing.T) {
	t.Skip("CPA reserve-settle dispatch pending — same gap as CPC")
}

func TestBillingViewabilityVCPMSettle(t *testing.T) {
	t.Skip("viewability events from /v1/t/view don't currently flow into pkg/billing — pending")
}

func TestBillingReservationExpiry(t *testing.T) {
	t.Skip("reservation expiry needs a cron helper (RunRollup-like) plus a low TTL config knob; pending")
}

func TestBillingTieredRevenueShareTierFlip(t *testing.T) {
	t.Skip("requires seeding tiered contracts via publishers.revshare_config + producing enough impressions to cross a tier; can build once we add a 'bulk auctions' harness helper")
}

func TestBillingGuaranteedMinimumSubsidy(t *testing.T) {
	t.Skip("guaranteed minimum needs a contract with GuaranteedMinCPM > clearing — seedable but pending a contract-write helper")
}

func TestBillingDealTypeFeeModifier(t *testing.T) {
	t.Skip("deal_type fee modifier in revshare_config — needs the same contract-write helper")
}

func TestBillingCurrencyConversion(t *testing.T) {
	t.Skip("multi-currency flow needs exchange_rates table seeded + a non-USD campaign; pending")
}
