//go:build e2e

// Data-monetization gate (ADR 0009 phase 1) — an EXTERNAL buyer's win on a
// request carrying a fee-bearing segment pays the segment owner EXACTLY, at
// impression time, through the real money spine:
//
//	label+fee (gateway) → SSP monetization map → user.data rides →
//	external win → DataFeeEvent → data_fee_pending → impression →
//	data_fee_earnings + ledger double-entry + owner balance credit
//
// Money math under the default 30% margin, fee $0.50 CPM = 500,000µ:
// per-impression fee 500µ, platform margin 150µ, owner net 350µ. The test
// derives the split from the LIVE reporting.data_fee_margin_pct rather than
// hardcoding 30, so an ops-tuned stack doesn't fail with the accrual correct.
package e2e

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestDataFeePaysOwnerOnExternalWin(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "datafee")

	// External buyer that WINS: bids 8.00, comfortably above the internal
	// campaign's 3.50. Its seat ("fake-seat") is not a tenant UUID — exactly
	// how the SSP recognises an external winner.
	fake := harness.NewFakeDSP(t, harness.FakeDSPOpts{Mode: harness.FakeDSPBidder, BidPrice: 8.0})
	h.SetConfigForPod(t, "exchange.dsp_endpoints",
		fake.URL+","+h.URLs.ClusterDSP, harness.PodExchange)
	t.Cleanup(func() {
		h.SetConfigForPod(t, "exchange.dsp_endpoints",
			h.URLs.ClusterDSP+","+h.URLs.ClusterDSPComp1+","+h.URLs.ClusterDSPComp2, harness.PodExchange)
	})

	// A public segment with one member, IAB-labelled and carrying a $0.50
	// CPM data fee. The world's advertiser account is the data OWNER.
	const feeMicros = 500_000 // $0.50 CPM → 500µ per impression
	member := fmt.Sprintf("datafee-member-%d", time.Now().UnixNano())
	segID := h.UploadAudience(t, w.AdvAcc.ID, "datafee-suv-intenders", "public", []string{member})
	h.SetSegmentTaxonomy(t, w.AdvAcc.ID, segID, 13)
	h.SetSegmentDataFee(t, w.AdvAcc.ID, segID, feeMicros)
	h.RefreshAllCaches(t)

	balance := func() float64 {
		var b float64
		if err := h.DB.QueryRow(
			`SELECT COALESCE(balance, 0) FROM advertiser_balances WHERE account_id = $1::uuid`,
			w.AdvAcc.ID).Scan(&b); err != nil {
			t.Fatalf("balance read: %v", err)
		}
		return b
	}
	balanceBefore := balance()

	// Expected split under the margin the reporting pods actually read. Every
	// reporting replica seeds its schema default into a per-pod config row at
	// boot, so rows exist on a healthy stack; divergent values across pods
	// would make the split depend on which replica consumes the impression —
	// fail loudly rather than assert a coin flip.
	marginPct := 30.0 // schema default — used only if no pod has seeded yet
	{
		rows, err := h.DB.Query(
			`SELECT DISTINCT value FROM config WHERE key = 'reporting.data_fee_margin_pct'`)
		if err != nil {
			t.Fatalf("margin config read: %v", err)
		}
		var values []string
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("margin config scan: %v", err)
			}
			values = append(values, v)
		}
		rows.Close()
		if len(values) > 1 {
			t.Fatalf("reporting.data_fee_margin_pct diverges across pods (%v) — split indeterminate", values)
		}
		if len(values) == 1 {
			// config.value is jsonb — a float key serializes as a JSON string
			// ("30"), so strip the surrounding quotes before parsing.
			raw := strings.Trim(values[0], `"`)
			f, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				t.Fatalf("margin config value %q: %v", values[0], err)
			}
			marginPct = f
		}
	}
	// Mirrors the accrual's integer math: margin = floor(fee × pct/100).
	expMargin := int64(500.0 * marginPct / 100)
	expNet := int64(500) - expMargin

	// Win as the member; retry briefly while the SSP monetization map and
	// the fee event propagate (same async invalidate as the taxonomy stamp).
	var trace string
	var winner harness.BidResponseWinner
	deadline := time.Now().Add(15 * time.Second)
	for trace == "" && time.Now().Before(deadline) {
		res := h.RunAuctionWith(t, harness.AuctionParams{
			Placement: w.Placement.ExternalID, Geo: "GBR", Device: "mobile", UserID: member,
		})
		winner = h.ExtractWinner(t, res)
		if winner.NoBid || winner.Seat != "fake-seat" {
			time.Sleep(time.Second)
			continue
		}
		// External win — the attribution record should be parked. Poll
		// briefly (the SSP publishes fire-and-forget).
		for i := 0; i < 20 && trace == ""; i++ {
			var n int
			if err := h.DB.QueryRow(
				`SELECT count(*) FROM data_fee_pending WHERE trace_id = $1`, res.TraceID).Scan(&n); err != nil {
				t.Fatalf("pending lookup: %v", err)
			}
			if n == 1 {
				trace = res.TraceID
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
		if trace == "" {
			time.Sleep(time.Second) // map not warm yet on the serving pod — try a fresh auction
		}
	}
	if trace == "" {
		t.Fatalf("no data_fee_pending row after external wins (last winner: nobid=%v seat=%q) — SSP attribution did not fire", winner.NoBid, winner.Seat)
	}

	// Deliver the impression → accrual settles.
	h.FireImpression(t, trace, winner.CampaignID, winner.CreativeID,
		w.Placement.ID, w.Publisher.ID, winner.Seat, "USD", winner.Price)

	var feeSum, netSum, marginSum int64
	harness.WaitFor(t, 20*time.Second, "data fee accrued", func() bool {
		err := h.DB.QueryRow(`
SELECT COALESCE(SUM(fee_micros),0), COALESCE(SUM(owner_net_micros),0), COALESCE(SUM(margin_micros),0)
FROM data_fee_earnings WHERE trace_id = $1 AND account_id = $2::uuid`,
			trace, w.AdvAcc.ID).Scan(&feeSum, &netSum, &marginSum)
		return err == nil && feeSum > 0
	})

	// EXACT money: 500µ fee splits into margin + owner net at the live pct
	// (150µ/350µ under the default 30%).
	if feeSum != 500 || marginSum != expMargin || netSum != expNet {
		t.Errorf("accrual micros = fee %d / margin %d / net %d, want 500/%d/%d",
			feeSum, marginSum, netSum, expMargin, expNet)
	}

	// The owner's spendable balance grew by exactly the net.
	wantDelta := float64(expNet) / 1e6
	if delta := balance() - balanceBefore; !almostEqual(delta, wantDelta, 1e-9) {
		t.Errorf("owner balance delta = %.9f, want %.9f", delta, wantDelta)
	}

	// Ledger double-entry balances: extseat debit == owner credit + margin credit.
	var debits, credits float64
	if err := h.DB.QueryRow(`
SELECT COALESCE(SUM(CASE WHEN entry_type='debit'  THEN amount END),0),
       COALESCE(SUM(CASE WHEN entry_type='credit' THEN amount END),0)
FROM ledger_entries WHERE reference_type='data_fee' AND reference_id LIKE $1 || '%'`,
		trace).Scan(&debits, &credits); err != nil {
		t.Fatalf("ledger query: %v", err)
	}
	if !almostEqual(debits, credits, 1e-9) || !almostEqual(debits, 0.0005, 1e-9) {
		t.Errorf("ledger debits %.9f / credits %.9f, want both 0.000500", debits, credits)
	}

	// Redelivery safety: the pending row is gone, so a duplicate impression
	// cannot double-accrue.
	var left int
	if err := h.DB.QueryRow(`SELECT count(*) FROM data_fee_pending WHERE trace_id = $1`, trace).Scan(&left); err != nil {
		t.Fatalf("pending recheck: %v", err)
	}
	if left != 0 {
		t.Errorf("pending row still present after accrual — double-accrual risk")
	}
	h.FireImpression(t, trace, winner.CampaignID, winner.CreativeID,
		w.Placement.ID, w.Publisher.ID, winner.Seat, "USD", winner.Price)
	// Positively confirm the redelivery was PROCESSED before asserting no growth:
	// the tracker dedups the repeat trace and publishes adtech.tracker.rejected
	// reason=dedup. Waiting on that (not a blind fixed sleep) closes the false-pass
	// window where a slow double-accrual lands AFTER the sleep yet the equality
	// check still passes.
	harness.WaitFor(t, 15*time.Second, "duplicate impression deduped by tracker", func() bool {
		return len(h.TrackerRejectionsByTrace(t, trace, "dedup")) >= 1
	})
	var feeSum2 int64
	if err := h.DB.QueryRow(
		`SELECT COALESCE(SUM(fee_micros),0) FROM data_fee_earnings WHERE trace_id = $1`, trace).Scan(&feeSum2); err != nil {
		t.Fatalf("earnings recheck: %v", err)
	}
	if feeSum2 != feeSum {
		t.Errorf("earnings grew on duplicate impression: %d → %d", feeSum, feeSum2)
	}

	// Negative: a user in NO segment → no user.data → nothing parked, even
	// though the same external buyer wins.
	res := h.RunAuctionWith(t, harness.AuctionParams{
		Placement: w.Placement.ExternalID, Geo: "GBR", Device: "mobile", UserID: member + "-nobody",
	})
	if wnr := h.ExtractWinner(t, res); !wnr.NoBid {
		var n int
		if err := h.DB.QueryRow(`SELECT count(*) FROM data_fee_pending WHERE trace_id = $1`, res.TraceID).Scan(&n); err != nil {
			t.Fatalf("negative pending lookup: %v", err)
		}
		if n != 0 {
			t.Errorf("segment-less auction parked a fee attribution — over-attribution")
		}
	}
}

func almostEqual(a, b, eps float64) bool {
	d := a - b
	return d < eps && d > -eps
}
