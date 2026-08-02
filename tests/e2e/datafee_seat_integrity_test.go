//go:build e2e

// Data-fee seat integrity (segtax gap close): the data-monetization receivable
// must be attributed to the TRUSTED seat bound to which configured endpoint won
// — never the seat the bidder self-declares in its response, which it could set
// to dodge the fee (empty/UUID → old code skipped it as "internal") or misdirect
// it onto a competitor. Here an external bidder self-declares a UUID seat (the
// evasion vector) at an endpoint whose operator-configured ;seat= is
// "trusted-acme"; the fee must still accrue AND land on "trusted-acme".
package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestDataFeeSeatIntegrityBillsTrustedSeat(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "df-seat")

	const trustedSeat = "trusted-acme"
	// The bidder LIES: it self-declares a UUID-shaped seat. Under the old code
	// the SSP saw a UUID and treated the win as internal → the fee was dodged.
	const spoofedSeat = "00000000-0000-0000-0000-000000000000"
	fake := harness.NewFakeDSP(t, harness.FakeDSPOpts{Mode: harness.FakeDSPBidder, BidPrice: 8.0, Seat: spoofedSeat})

	// The operator binds the trusted billable seat to the endpoint via ;seat=.
	h.SetConfigForPod(t, "exchange.dsp_endpoints",
		fake.URL+";seat="+trustedSeat+","+h.URLs.ClusterDSP, harness.PodExchange)
	t.Cleanup(func() {
		h.SetConfigForPod(t, "exchange.dsp_endpoints",
			h.URLs.ClusterDSP+","+h.URLs.ClusterDSPComp1+","+h.URLs.ClusterDSPComp2, harness.PodExchange)
	})

	const feeMicros = 500_000 // $0.50 CPM
	member := fmt.Sprintf("df-seat-member-%d", time.Now().UnixNano())
	segID := h.UploadAudience(t, w.AdvAcc.ID, "df-seat-intenders", "public", []string{member})
	h.SetSegmentTaxonomy(t, w.AdvAcc.ID, segID, 13)
	h.SetSegmentDataFee(t, w.AdvAcc.ID, segID, feeMicros)
	h.RefreshAllCaches(t)

	// Win as the member; retry while the SSP monetization map + fee event
	// propagate. A parked data_fee_pending row proves the fee was NOT dodged
	// despite the UUID self-declared seat (the evasion proof).
	var trace string
	var winner harness.BidResponseWinner
	deadline := time.Now().Add(20 * time.Second)
	for trace == "" && time.Now().Before(deadline) {
		res := h.RunAuctionWith(t, harness.AuctionParams{
			Placement: w.Placement.ExternalID, Geo: "GBR", Device: "mobile", UserID: member,
		})
		winner = h.ExtractWinner(t, res)
		if winner.NoBid {
			time.Sleep(time.Second)
			continue
		}
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
			time.Sleep(time.Second)
		}
	}
	if trace == "" {
		t.Fatalf("no data_fee_pending after external wins (last winner nobid=%v seat=%q) — a UUID-declared seat DODGED the fee",
			winner.NoBid, winner.Seat)
	}

	// Deliver the impression → the accrual settles.
	h.FireImpression(t, trace, winner.CampaignID, winner.CreativeID,
		w.Placement.ID, w.Publisher.ID, winner.Seat, "USD", winner.Price)

	// The earning must be attributed to the TRUSTED seat, not the self-declared UUID.
	var gotSeat string
	var feeSum int64
	harness.WaitFor(t, 20*time.Second, "data fee accrued to the trusted seat", func() bool {
		err := h.DB.QueryRow(`
SELECT COALESCE(winner_seat,''), COALESCE(SUM(fee_micros),0)
FROM data_fee_earnings WHERE trace_id = $1 GROUP BY winner_seat`,
			trace).Scan(&gotSeat, &feeSum)
		return err == nil && feeSum > 0
	})
	if gotSeat != trustedSeat {
		t.Errorf("data-fee earning attributed to seat %q, want %q (the trusted endpoint seat, not the self-declared %q)",
			gotSeat, trustedSeat, spoofedSeat)
	}
	if gotSeat == spoofedSeat {
		t.Errorf("earning attributed to the SELF-DECLARED seat %q — integrity hole open", spoofedSeat)
	}

	// And nothing landed under the spoofed seat.
	var spoofRows int
	if err := h.DB.QueryRow(
		`SELECT count(*) FROM data_fee_earnings WHERE trace_id=$1 AND winner_seat=$2`,
		trace, spoofedSeat).Scan(&spoofRows); err != nil {
		t.Fatalf("spoof-seat count: %v", err)
	}
	if spoofRows != 0 {
		t.Errorf("%d earning rows under the spoofed seat %q; want 0", spoofRows, spoofedSeat)
	}
}
