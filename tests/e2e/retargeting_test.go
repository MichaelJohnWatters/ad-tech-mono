//go:build e2e

// Retargeting pixel — the targeting-data-flow diagram's last planned piece:
// an advertiser embeds /v1/t/rt on THEIR site; visits become consent-gated
// site_visit behaviour rows attributed to the advertiser's account + tag;
// a behavioural rule turns them into retargeting-segment memberships that
// gate real auctions. Also proves: GPC visits capture nothing, and another
// account using the same tag name enrolls nobody (account scoping).
package e2e

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestRetargetingPixel(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "retarget")
	uniq := time.Now().UnixNano()
	visitor := fmt.Sprintf("rt-visitor-%d", uniq)
	gpcUser := fmt.Sprintf("rt-gpc-%d", uniq)
	stranger := fmt.Sprintf("rt-stranger-%d", uniq)
	tag := fmt.Sprintf("product-page-%d", uniq)

	fire := func(uid, extra string) {
		t.Helper()
		url := fmt.Sprintf("http://localhost:8083/v1/t/rt?aid=%s&tag=%s&uid=%s%s", w.AdvAcc.ID, tag, uid, extra)
		resp, err := http.Get(url)
		if err != nil {
			t.Fatalf("pixel fire: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("pixel status %d", resp.StatusCode)
		}
	}
	// Two consented visits; one GPC do-not-sell visit.
	fire(visitor, "")
	fire(visitor, "")
	fire(gpcUser, "&gpc=1")

	// Retargeting rule: ≥2 site visits with this tag. Plus the SAME rule
	// owned by a DIFFERENT account (the publisher's) — its members must
	// stay empty, because site_visit rows are scoped to the pixel's aid.
	mkSeg := func(accountID string) string {
		var id string
		if err := h.DB.QueryRow(`
INSERT INTO audience_segments (account_id, name, type, status, source, visibility, rule)
VALUES ($1::uuid, $2, 'retargeting', 'active', 'profile_builder', 'public',
        jsonb_build_object('event', 'site_visit', 'tag', $3::text, 'min_count', 2))
RETURNING id::text`, accountID, fmt.Sprintf("rt-seg-%s-%d", accountID[:8], uniq), tag).Scan(&id); err != nil {
			t.Fatalf("create segment: %v", err)
		}
		return id
	}
	segMine := mkSeg(w.AdvAcc.ID)
	segTheirs := mkSeg(w.PubAcc.ID)

	deadline := time.Now().Add(45 * time.Second)
	for h.LakeResidual(t, visitor)["behaviour_signals"] < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("site_visit rows never landed: %v", h.LakeResidual(t, visitor))
		}
		time.Sleep(2 * time.Second)
	}
	// The GPC visit must not have produced a row.
	if n := h.LakeResidual(t, gpcUser)["behaviour_signals"]; n != 0 {
		t.Errorf("GPC visit captured %d rows, want 0", n)
	}

	runBuilder(t, h, lakeStore(t, h))

	member := func(segID, userID string) bool {
		var n int
		if err := h.DB.QueryRow(`SELECT count(*) FROM audience_segment_members WHERE segment_id = $1 AND user_id = $2`,
			segID, userID).Scan(&n); err != nil {
			t.Fatalf("membership query: %v", err)
		}
		return n > 0
	}
	if !member(segMine, visitor) {
		t.Error("visitor not enrolled in the advertiser's retargeting segment")
	}
	if member(segTheirs, visitor) {
		t.Error("visitor enrolled in ANOTHER account's segment — site_visit account scoping broken")
	}

	// The retargeting segment gates a real auction.
	setTargeting(t, h, w.Campaign.ID, "include_segments", pq.StringArray{segMine})
	setTargeting(t, h, w.Campaign.ID, "include_geo", pq.StringArray{})
	setTargeting(t, h, w.Campaign.ID, "include_device", pq.StringArray{})
	h.RefreshAllCaches(t)
	win := h.ExtractWinner(t, h.RunAuctionWith(t, harness.AuctionParams{
		Placement: w.Placement.ExternalID, Geo: "GBR", Device: "mobile", UserID: visitor,
	}))
	if win.NoBid || win.CampaignID != w.Campaign.ID {
		t.Errorf("retargeted visitor auction: %+v, want campaign %s", win, w.Campaign.ID)
	}
	if !h.ExtractWinner(t, h.RunAuctionWith(t, harness.AuctionParams{
		Placement: w.Placement.ExternalID, Geo: "GBR", Device: "mobile", UserID: stranger,
	})).NoBid {
		t.Error("non-visitor won the retargeting-gated auction")
	}
}
