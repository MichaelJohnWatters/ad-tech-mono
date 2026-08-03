//go:build e2e

// Real-time retargeting: the abandoned-cart latency fix. The existing
// TestRetargetingPixel proves the BATCH path (fire pixel → run the profile-builder
// → enrolled). This proves the REAL-TIME path: firing the pixel once enrolls the
// visitor within seconds via cmd/audience-rt — WITHOUT running the batch builder —
// so the DSP retargets on the next request; and a purchase suppresses them so we
// stop chasing a buyer. Membership lands in Postgres directly (the deterministic
// proof); RefreshAllCaches then makes the auction assertions cache-independent.
package e2e

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestRealTimeRetargetingEnrollAndSuppress(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "rt-realtime")
	uniq := time.Now().UnixNano()
	visitor := fmt.Sprintf("rrt-visitor-%d", uniq)
	tag := fmt.Sprintf("cart-%d", uniq)

	// A single-visit retargeting rule (min_count 1) — the instant case audience-rt
	// handles. Public visibility so the SSP stamps it and the auction can gate on it.
	var segID string
	if err := h.DB.QueryRow(`
INSERT INTO audience_segments (account_id, name, type, status, source, visibility, rule)
VALUES ($1::uuid, $2, 'retargeting', 'active', 'profile_builder', 'public',
        jsonb_build_object('event', 'site_visit', 'tag', $3::text, 'min_count', 1))
RETURNING id::text`, w.AdvAcc.ID, fmt.Sprintf("rrt-seg-%d", uniq), tag).Scan(&segID); err != nil {
		t.Fatalf("create retargeting segment: %v", err)
	}

	// Campaign bids ONLY for members of the retargeting segment.
	setTargeting(t, h, w.Campaign.ID, "include_segments", pq.StringArray{segID})
	setTargeting(t, h, w.Campaign.ID, "include_geo", pq.StringArray{})
	setTargeting(t, h, w.Campaign.ID, "include_device", pq.StringArray{})

	member := func() bool {
		var n int
		if err := h.DB.QueryRow(`SELECT count(*) FROM audience_segment_members WHERE segment_id = $1 AND user_id = $2`,
			segID, visitor).Scan(&n); err != nil {
			t.Fatalf("membership query: %v", err)
		}
		return n > 0
	}
	waitMember := func(want bool, what string) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for member() != want {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for membership=%v (%s)", want, what)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	retargets := func() bool {
		h.RefreshAllCaches(t)
		win := h.ExtractWinner(t, h.RunAuctionWith(t, harness.AuctionParams{
			Placement: w.Placement.ExternalID, Geo: "GBR", Device: "mobile", UserID: visitor,
		}))
		return !win.NoBid && win.CampaignID == w.Campaign.ID
	}

	// Before any visit: not a member, and the campaign doesn't bid for this user.
	if member() {
		t.Fatal("visitor already a member before visiting")
	}
	if retargets() {
		t.Fatal("campaign retargeted a visitor who never visited")
	}

	// One retargeting-pixel visit. audience-rt should enroll within SECONDS —
	// note we DO NOT run the batch profile-builder anywhere in this test.
	fireVisit(t, w.AdvAcc.ID, tag, visitor)
	waitMember(true, "enroll on visit")
	if !retargets() {
		t.Error("DSP did not retarget the freshly-enrolled visitor")
	}

	// TTL: the enrolled member carries an expiry ~30 days out (the default
	// window) so an abandoner who never converts ages out instead of being
	// chased forever.
	var expiresIn *float64
	if err := h.DB.QueryRow(`SELECT extract(epoch FROM (expires_at - now())) FROM audience_segment_members WHERE segment_id = $1 AND user_id = $2`,
		segID, visitor).Scan(&expiresIn); err != nil {
		t.Fatalf("expires_at query: %v", err)
	}
	if expiresIn == nil {
		t.Error("enrolled member has no expires_at — real-time members must carry a TTL")
	} else if d := time.Duration(*expiresIn) * time.Second; d < 29*24*time.Hour || d > 31*24*time.Hour {
		t.Errorf("member expires in %v, want ~30d", d)
	}

	// Age the member out (simulate the window elapsing): once expired, the read
	// paths exclude it, so the DSP stops retargeting even before any purge.
	if _, err := h.DB.Exec(`UPDATE audience_segment_members SET expires_at = now() - interval '1 minute' WHERE segment_id = $1 AND user_id = $2`,
		segID, visitor); err != nil {
		t.Fatalf("expire member: %v", err)
	}
	if retargets() {
		t.Error("DSP retargeted an EXPIRED member — TTL read-side filtering not applied")
	}
	// Re-visiting refreshes the window, bringing them back.
	if _, err := h.DB.Exec(`DELETE FROM audience_segment_members WHERE segment_id = $1 AND user_id = $2`, segID, visitor); err != nil {
		t.Fatalf("reset member: %v", err)
	}
	fireVisit(t, w.AdvAcc.ID, tag, visitor)
	waitMember(true, "re-enroll on repeat visit")

	// The shopper buys → a purchase conversion suppresses them so we stop paying
	// to chase a converted user.
	h.FireConversionForVisitor(t, fmt.Sprintf("rrt-conv-%d", uniq), w.AdvAcc.ID, visitor, "purchase", "USD", 42.00)
	waitMember(false, "suppress on purchase")
	if retargets() {
		t.Error("DSP still retargeted a visitor who already converted")
	}
}

func fireVisit(t *testing.T, advAccID, tag, uid string) {
	t.Helper()
	url := fmt.Sprintf("http://localhost:8083/v1/t/rt?aid=%s&tag=%s&uid=%s", advAccID, tag, uid)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("retargeting pixel fire: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("retargeting pixel status %d", resp.StatusCode)
	}
}
