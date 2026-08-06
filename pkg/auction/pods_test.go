package auction_test

import (
	"context"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auction"
)

// Helper: build a bid with the fields the pod tests care about.
func bid(dsp, camp, cr string, price float64, dur int, advID, cat string) auction.Bid {
	return auction.Bid{
		DSPID: dsp, CampaignID: camp, CreativeID: cr,
		Price: price, Duration: dur,
		AdvertiserID: advID, Category: cat,
	}
}

func podReq(p *auction.PodRequest) auction.AuctionRequest {
	return auction.AuctionRequest{
		Channel: "video", Format: "pod",
		Pod: p,
	}
}

// Variable-duration pod with enough demand to fill: three 15s bids
// into a 60s break, separation off. Best-CPM/sec order wins.
func TestPod_Variable_FullFill(t *testing.T) {
	bids := []auction.Bid{
		bid("d", "c1", "cr1", 6.0, 15, "adv-a", ""), // 0.40 CPM/sec
		bid("d", "c2", "cr2", 9.0, 15, "adv-b", ""), // 0.60 CPM/sec — best
		bid("d", "c3", "cr3", 3.0, 15, "adv-c", ""), // 0.20 CPM/sec
		bid("d", "c4", "cr4", 4.0, 30, "adv-d", ""), // 0.133 CPM/sec
	}
	req := podReq(&auction.PodRequest{MaxDuration: 60})

	s := &auction.PodStrategy{}
	r, err := s.Select(context.Background(), bids, req)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(r.Winners) != 3 {
		t.Fatalf("expected 3 winners, got %d (%+v)", len(r.Winners), r.Winners)
	}
	// Winners should be CPM/sec descending: c2 (0.60), c1 (0.40), c3 (0.20)
	// with positions 1,2,3.
	wantOrder := []string{"c2", "c1", "c3"}
	for i, w := range r.Winners {
		if w.Bid.CampaignID != wantOrder[i] {
			t.Errorf("position %d: got %q, want %q", i+1, w.Bid.CampaignID, wantOrder[i])
		}
		if w.Position != i+1 {
			t.Errorf("position field = %d, want %d", w.Position, i+1)
		}
	}
	if r.ShortFill != 15 {
		t.Errorf("ShortFill = %d, want 15 (60s pod - 3×15s)", r.ShortFill)
	}
	if !r.IsMultiWinner {
		t.Error("IsMultiWinner should be true for pod result")
	}
}

// Variable-duration pod with not enough demand: 30s break, only one
// 15s bid available. ShortFill captures the leftover seconds.
func TestPod_Variable_ShortFill(t *testing.T) {
	bids := []auction.Bid{bid("d", "c1", "cr1", 6.0, 15, "adv-a", "")}
	req := podReq(&auction.PodRequest{MaxDuration: 30})
	r, err := (&auction.PodStrategy{}).Select(context.Background(), bids, req)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(r.Winners) != 1 || r.ShortFill != 15 {
		t.Errorf("winners=%d ShortFill=%d, want 1/15 (%+v)", len(r.Winners), r.ShortFill, r)
	}
}

// NoSameAdvertiserAdjacent: two highest-CPM bids share an advertiser.
// The second is skipped (separation), a lower-CPM third bid fills
// the slot, and the skipped bid lands in LossBids with the
// competitive-separation reason.
func TestPod_Variable_NoSameAdvertiserAdjacent(t *testing.T) {
	bids := []auction.Bid{
		bid("d", "c1", "cr1", 9.0, 15, "adv-a", ""), // best — wins slot 1
		bid("d", "c2", "cr2", 8.0, 15, "adv-a", ""), // 2nd best but same advertiser as c1 → blocked
		bid("d", "c3", "cr3", 5.0, 15, "adv-b", ""), // takes slot 2
	}
	req := podReq(&auction.PodRequest{MaxDuration: 30, NoSameAdvertiserAdjacent: true})
	r, err := (&auction.PodStrategy{}).Select(context.Background(), bids, req)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(r.Winners) != 2 {
		t.Fatalf("expected 2 winners, got %d", len(r.Winners))
	}
	if r.Winners[0].Bid.CampaignID != "c1" || r.Winners[1].Bid.CampaignID != "c3" {
		t.Errorf("winner order = %+v", r.Winners)
	}
	var blockReasons int
	for _, l := range r.LossBids {
		if l.Bid.CampaignID == "c2" && l.Reason == auction.LossBlocked {
			blockReasons++
		}
	}
	if blockReasons != 1 {
		t.Errorf("expected c2 in LossBids as LossBlocked, got %+v", r.LossBids)
	}
}

// UniqueAdvertiser is stricter: at most one bid per advertiser anywhere
// in the pod (not just adjacent). Same setup as the adjacency test
// produces the same result here.
func TestPod_Variable_UniqueAdvertiser(t *testing.T) {
	bids := []auction.Bid{
		bid("d", "c1", "cr1", 9.0, 15, "adv-a", ""),
		bid("d", "c2", "cr2", 8.0, 15, "adv-a", ""), // dupe advertiser
		bid("d", "c3", "cr3", 5.0, 15, "adv-b", ""),
	}
	req := podReq(&auction.PodRequest{MaxDuration: 30, UniqueAdvertiser: true})
	r, _ := (&auction.PodStrategy{}).Select(context.Background(), bids, req)
	for _, w := range r.Winners {
		if w.Bid.CampaignID == "c2" {
			t.Errorf("c2 should be blocked by UniqueAdvertiser; got winners %+v", r.Winners)
		}
	}
}

// Fixed-slot pod (SSAI-style): RqdDurs = [15, 30]. Best 15s wins slot 1,
// best 30s wins slot 2; a 60s bid finds no slot and falls out.
func TestPod_Fixed_SlotMatching(t *testing.T) {
	bids := []auction.Bid{
		bid("d", "c-15-a", "cr1", 4.0, 15, "adv-a", ""),  // best 15s CPM/sec
		bid("d", "c-15-b", "cr2", 3.0, 15, "adv-b", ""),  // backup 15s
		bid("d", "c-30-a", "cr3", 12.0, 30, "adv-c", ""), // best 30s
		bid("d", "c-60", "cr4", 30.0, 60, "adv-d", ""),   // no matching slot
	}
	req := podReq(&auction.PodRequest{RqdDurs: []int{15, 30}})
	r, err := (&auction.PodStrategy{}).Select(context.Background(), bids, req)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(r.Winners) != 2 {
		t.Fatalf("expected 2 winners, got %d", len(r.Winners))
	}
	if r.Winners[0].Bid.CampaignID != "c-15-a" {
		t.Errorf("slot 1 winner = %q, want c-15-a", r.Winners[0].Bid.CampaignID)
	}
	if r.Winners[1].Bid.CampaignID != "c-30-a" {
		t.Errorf("slot 2 winner = %q, want c-30-a", r.Winners[1].Bid.CampaignID)
	}
	// c-60 must be in LossBids.
	found := false
	for _, l := range r.LossBids {
		if l.Bid.CampaignID == "c-60" {
			found = true
		}
	}
	if !found {
		t.Errorf("c-60 should be in LossBids: %+v", r.LossBids)
	}
}

// MinCPMPerSec floor filters out bids whose CPM/sec is below the
// per-pod floor. The high-CPM/sec bid wins; the cheap-per-sec bid is
// blocked even though it would fit.
func TestPod_Variable_MinCPMPerSecFloor(t *testing.T) {
	bids := []auction.Bid{
		bid("d", "premium", "cr1", 9.0, 15, "adv-a", ""), // 0.60 CPM/sec — above floor
		bid("d", "cheap", "cr2", 3.0, 30, "adv-b", ""),   // 0.10 CPM/sec — below 0.30 floor
	}
	req := podReq(&auction.PodRequest{MaxDuration: 60, MinCPMPerSec: 0.30})
	r, _ := (&auction.PodStrategy{}).Select(context.Background(), bids, req)
	for _, w := range r.Winners {
		if w.Bid.CampaignID == "cheap" {
			t.Errorf("cheap (0.10/sec) should be filtered by MinCPMPerSec=0.30; got %+v", r.Winners)
		}
	}
	if len(r.Winners) != 1 || r.Winners[0].Bid.CampaignID != "premium" {
		t.Errorf("want premium as sole winner; got %+v", r.Winners)
	}
}

// MaxAds caps the slot count even when remaining duration could fit
// more bids. Useful for radio breaks where the publisher contractually
// caps the spot count regardless of available time.
func TestPod_Variable_MaxAdsCap(t *testing.T) {
	bids := []auction.Bid{
		bid("d", "c1", "cr1", 9, 15, "a", ""),
		bid("d", "c2", "cr2", 8, 15, "b", ""),
		bid("d", "c3", "cr3", 7, 15, "c", ""),
		bid("d", "c4", "cr4", 6, 15, "d", ""),
	}
	req := podReq(&auction.PodRequest{MaxDuration: 60, MaxAds: 2})
	r, _ := (&auction.PodStrategy{}).Select(context.Background(), bids, req)
	if len(r.Winners) != 2 {
		t.Errorf("MaxAds=2 should cap winners; got %d", len(r.Winners))
	}
}

// No bids → ErrNoBids. Mirrors single-winner behaviour.
func TestPod_NoBids(t *testing.T) {
	req := podReq(&auction.PodRequest{MaxDuration: 30})
	_, err := (&auction.PodStrategy{}).Select(context.Background(), nil, req)
	if err != auction.ErrNoBids {
		t.Errorf("expected ErrNoBids, got %v", err)
	}
}

// Pod strategy invoked without Pod context → falls back to
// single-winner so a misrouted request still returns a usable result.
func TestPod_FallbackToSingleWinnerWhenNoPodContext(t *testing.T) {
	bids := []auction.Bid{
		bid("d", "c1", "cr1", 5, 15, "a", ""),
		bid("d", "c2", "cr2", 8, 15, "b", ""),
	}
	req := auction.AuctionRequest{Channel: "video", Format: "pod"} // Pod nil
	r, err := (&auction.PodStrategy{}).Select(context.Background(), bids, req)
	if err != nil {
		t.Fatalf("fallback: %v", err)
	}
	if len(r.Winners) != 1 || r.Winners[0].Bid.CampaignID != "c2" {
		t.Errorf("expected single-winner fallback to pick c2; got %+v", r.Winners)
	}
}
