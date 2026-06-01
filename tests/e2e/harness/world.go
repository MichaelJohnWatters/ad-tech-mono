//go:build e2e

package harness

import "testing"

// World is the fixture every focused test starts from: one publisher with one
// placement, one advertiser with one campaign live and targeted at GBR/mobile.
// Mirrors the state that TestEndToEnd builds across steps 01–05.
//
// Returned by BuildBasicWorld so per-feature tests don't repeat 30 lines of
// setup boilerplate.
type World struct {
	Admin     Account
	PubAcc    Account
	AdvAcc    Account
	Publisher Publisher
	Placement Placement
	IO        InsertionOrder
	Campaign  Campaign
}

// BuildBasicWorld resets state and creates a minimal end-to-end setup:
// admin, publisher account + inventory, advertiser + IO + campaign. The
// campaign is live at base bid 3.50, daily budget 500, geo=GBR, device=mobile.
//
// Advertiser external key — the DSP service filters its warm cache to a
// hardcoded allowlist of advertisers read from profiles/dsps/internal.yaml
// (see derivedAccountUUIDs in cmd/dsp/main.go). We reuse one of those
// known external keys ("adv-acme") so the DSP picks up our test campaign
// after RefreshAllCaches. Without this, the DSP cache silently drops the
// test campaign and every auction returns NoBid.
//
// Suffix only affects publisher/placement/IO/campaign external keys so
// concurrent tests don't collide on those — they all share the advertiser
// account, which is safe because Reset wipes between tests.
//
// Callers can mutate or extend the returned handles (e.g. SetCampaignDailyBudget)
// to set up a specific scenario without re-creating accounts.
func BuildBasicWorld(t *testing.T, h *Harness, suffix string) World {
	t.Helper()
	h.Reset(t)

	w := World{}
	w.Admin = h.CreateAdmin(t, "e2e-admin-"+suffix)
	w.PubAcc = h.CreatePublisher(t, "e2e-pub-"+suffix)
	w.Publisher = h.AddPublisher(t, w.PubAcc, "e2e-pub-"+suffix, "e2e-"+suffix+".test")
	w.Placement = h.AddPlacement(t, w.Publisher, "e2e-pl-"+suffix,
		300, 250, 1.00, []string{"IAB12"})

	// "adv-acme" is in internal DSP's allowlist (profiles/dsps/internal.yaml).
	// Required for the campaign to land in the DSP warm cache.
	w.AdvAcc = h.CreateAdvertiser(t, "adv-acme")
	w.IO = h.CreateInsertionOrder(t, w.AdvAcc, "e2e-io-"+suffix, 5000)
	w.Campaign = h.CreateCampaign(t, w.AdvAcc, w.IO,
		"e2e-li-"+suffix,
		3.50,  // base bid
		500,   // daily budget
		"e2e-cr-"+suffix,
		"adv-"+suffix+".test",
		Targeting{Geos: []string{"GBR"}, Devices: []string{"mobile"}},
	)
	h.RefreshAllCaches(t)
	return w
}
