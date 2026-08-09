//go:build e2e

// Anonymous-guest household retargeting: a shopper who abandons a cart
// WITHOUT ever identifying themselves (no email, no login) is chaseable on
// the household. The tracker's rt pixel derives the shopper's household id
// (salted-IP hash, same derivation as the SSP) and audience-rt enrolls it
// alongside the visitor id (audience_rt.household_enroll); the DSP's
// household-keyed segment lookup then matches ANY device in the home — a
// different user id from the same IP gets the chase, no identity bridge
// anywhere. This is the guest-cart answer where the email moment never
// happens (docs/demos/RETARGETING-CHASE-DEMO.md covers the email path).
package e2e

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestAnonymousGuestHouseholdChase(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "rt-household")
	uniq := time.Now().UnixNano()
	guest := fmt.Sprintf("hh-guest-%d", uniq)   // the anonymous shop visitor
	other := fmt.Sprintf("hh-other-%d", uniq)   // a DIFFERENT device in the same home
	tag := fmt.Sprintf("cart-hh-%d", uniq)
	homeIP := fmt.Sprintf("198.51.100.%d", 1+uniq%254)  // TEST-NET-2: this household
	otherIP := fmt.Sprintf("198.51.100.%d", 1+(uniq+7)%254) // a different household
	if otherIP == homeIP {
		otherIP = "198.51.100.250"
	}

	var segID string
	if err := h.DB.QueryRow(`
INSERT INTO audience_segments (account_id, name, type, status, source, visibility, rule)
VALUES ($1::uuid, $2, 'retargeting', 'active', 'profile_builder', 'public',
        jsonb_build_object('event', 'site_visit', 'tag', $3::text, 'min_count', 1))
RETURNING id::text`, w.AdvAcc.ID, fmt.Sprintf("hh-seg-%d", uniq), tag).Scan(&segID); err != nil {
		t.Fatalf("create retargeting segment: %v", err)
	}
	setTargeting(t, h, w.Campaign.ID, "include_segments", pq.StringArray{segID})
	setTargeting(t, h, w.Campaign.ID, "include_geo", pq.StringArray{})
	setTargeting(t, h, w.Campaign.ID, "include_device", pq.StringArray{})

	householdMembers := func() int {
		var n int
		if err := h.DB.QueryRow(`SELECT count(*) FROM audience_segment_members WHERE segment_id = $1 AND user_id LIKE 'hh:%'`,
			segID).Scan(&n); err != nil {
			t.Fatalf("household membership query: %v", err)
		}
		return n
	}

	// The anonymous guest opens the cart. The pixel carries the shopper's IP
	// (?ip= — honoured because the test host is a private-range caller, the
	// same allowlist gate the SSP uses). NO email, NO identity anywhere.
	fireVisitWithIP(t, w.AdvAcc.ID, tag, guest, homeIP)

	// Within seconds: BOTH the visitor id and the derived household enrolled.
	deadline := time.Now().Add(30 * time.Second)
	for householdMembers() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("timed out: rt pixel visit never produced a household (hh:) enrollment")
		}
		time.Sleep(500 * time.Millisecond)
	}

	// The chase reaches a DIFFERENT user in the same household: the SSP
	// derives the same hh: id from the same IP, and the DSP's household-keyed
	// segment lookup matches. No shared user id, no email bridge.
	h.RefreshAllCaches(t)
	win := h.ExtractWinner(t, h.RunAuctionWith(t, harness.AuctionParams{
		Placement: w.Placement.ExternalID, Geo: "GBR", Device: "mobile",
		UserID: other, IP: homeIP,
	}))
	if win.NoBid || win.CampaignID != w.Campaign.ID {
		t.Fatalf("household chase failed: other-device user in the same home did not get the cart campaign (nobid=%v campaign=%s want %s)",
			win.NoBid, win.CampaignID, w.Campaign.ID)
	}

	// And it must NOT leak to a different household.
	winOther := h.ExtractWinner(t, h.RunAuctionWith(t, harness.AuctionParams{
		Placement: w.Placement.ExternalID, Geo: "GBR", Device: "mobile",
		UserID: fmt.Sprintf("hh-stranger-%d", uniq), IP: otherIP,
	}))
	if !winOther.NoBid && winOther.CampaignID == w.Campaign.ID {
		t.Fatal("household chase LEAKED to a different household")
	}

	// Current suppression semantics, asserted deliberately: a purchase by the
	// guest releases the guest's OWN id but the household row remains until
	// its TTL (person/household-level suppression is a recorded open operator
	// decision — see the audience pipeline memory/doc). If this assertion
	// starts failing because the household row is ALSO removed, that decision
	// shipped: update the demo doc and flip this check.
	h.FireConversionForVisitor(t, fmt.Sprintf("hh-conv-%d", uniq), w.AdvAcc.ID, guest, "purchase", "USD", 42.00)
	deadline = time.Now().Add(30 * time.Second)
	for {
		var n int
		if err := h.DB.QueryRow(`SELECT count(*) FROM audience_segment_members WHERE segment_id = $1 AND user_id = $2`,
			segID, guest).Scan(&n); err != nil {
			t.Fatalf("guest membership query: %v", err)
		}
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("purchase never suppressed the guest's own enrollment")
		}
		time.Sleep(500 * time.Millisecond)
	}
	if householdMembers() == 0 {
		t.Error("household row vanished on id-level suppression — if person/household suppression shipped, update this test + the demo doc")
	}
}

// fireVisitWithIP fires the rt pixel with an explicit shopper IP (?ip=),
// mirroring a server-side tag / local demo that legitimately knows the
// device address.
func fireVisitWithIP(t *testing.T, advAccID, tag, uid, ip string) {
	t.Helper()
	url := fmt.Sprintf("http://localhost:8083/v1/t/rt?aid=%s&tag=%s&uid=%s&ip=%s", advAccID, tag, uid, ip)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("retargeting pixel fire: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("retargeting pixel status %d", resp.StatusCode)
	}
}
