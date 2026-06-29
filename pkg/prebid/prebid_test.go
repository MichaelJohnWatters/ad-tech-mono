package prebid

import (
	"io"
	"log/slog"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
)

func silentLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestResolveFloor(t *testing.T) {
	cases := []struct {
		name      string
		inbound   float64
		ourMin    float64
		want      float64
	}{
		{"inbound higher wins", 5.00, 1.00, 5.00},
		{"our min higher wins", 0.50, 1.00, 1.00},
		{"equal returns inbound", 2.00, 2.00, 2.00},
		{"zero inbound takes our min", 0, 1.00, 1.00},
		{"zero min returns inbound", 3.00, 0, 3.00},
		{"both zero returns zero", 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveFloor(tc.inbound, tc.ourMin); got != tc.want {
				t.Errorf("ResolveFloor(%v, %v): got %v, want %v",
					tc.inbound, tc.ourMin, got, tc.want)
			}
		})
	}
}

func TestApplyFloorPolicy(t *testing.T) {
	t.Run("raises floors below platform min", func(t *testing.T) {
		req := &openrtb.BidRequest{
			Imp: []openrtb.Imp{
				{ID: "imp-1", BidFloor: 0.50},
				{ID: "imp-2", BidFloor: 5.00},
				{ID: "imp-3", BidFloor: 0},
			},
		}
		raised := ApplyFloorPolicy(req, 1.00, silentLog())
		if raised != 2 {
			t.Errorf("raised count: got %d, want 2", raised)
		}
		want := []float64{1.00, 5.00, 1.00}
		for i, w := range want {
			if req.Imp[i].BidFloor != w {
				t.Errorf("imp[%d].BidFloor: got %v, want %v",
					i, req.Imp[i].BidFloor, w)
			}
		}
	})

	t.Run("preserves deal IDs (opaque pass-through)", func(t *testing.T) {
		req := &openrtb.BidRequest{
			Imp: []openrtb.Imp{
				{ID: "imp-1", BidFloor: 5.00, DealID: "external-deal-abc"},
			},
		}
		ApplyFloorPolicy(req, 1.00, silentLog())
		if req.Imp[0].DealID != "external-deal-abc" {
			t.Errorf("DealID: got %q, want preserved", req.Imp[0].DealID)
		}
	})

	t.Run("no-op when all floors above min", func(t *testing.T) {
		req := &openrtb.BidRequest{
			Imp: []openrtb.Imp{
				{ID: "imp-1", BidFloor: 5.00},
				{ID: "imp-2", BidFloor: 3.00},
			},
		}
		raised := ApplyFloorPolicy(req, 1.00, silentLog())
		if raised != 0 {
			t.Errorf("raised count: got %d, want 0", raised)
		}
	})

	t.Run("tolerates nil logger", func(t *testing.T) {
		req := &openrtb.BidRequest{
			Imp: []openrtb.Imp{
				{ID: "imp-1", BidFloor: 0.50, DealID: "x"},
			},
		}
		// Should not panic with nil log even with a deal id present.
		ApplyFloorPolicy(req, 1.00, nil)
		if req.Imp[0].BidFloor != 1.00 {
			t.Errorf("BidFloor: got %v, want 1.00", req.Imp[0].BidFloor)
		}
	})
}

func TestBidderCodeIsLockedIn(t *testing.T) {
	// The bidder code is part of our public contract with external Prebid
	// integrations. Changing it would break every publisher who's already
	// configured "adtechmono" in their Prebid setup. This test fails loudly
	// if anyone touches it.
	if BidderCode != "adtechmono" {
		t.Errorf("BidderCode changed: got %q, want %q",
			BidderCode, "adtechmono")
	}
}
