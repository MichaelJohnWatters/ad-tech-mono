//go:build e2e

package harness

import (
	"database/sql"
	"encoding/json"
	"strconv"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/idgen"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/simulator/pages"
	"github.com/lib/pq"
)

// PagesWorld is a hermetic multi-format world for the external-pages e2e: one
// publisher with a placement for EVERY format (display/native/video/audio), and
// one live campaign per format on the DSP-allowlisted advertiser, all targeting
// USA/desktop (what VisitPage requests). PlacementByFormat maps each format to
// the external placement key that backs it, to hand to VisitPageWith.
type PagesWorld struct {
	Admin             Account
	PubAcc            Account
	AdvAcc            Account
	Publisher         Publisher
	PlacementByFormat map[pages.Format]string
}

// BuildPagesWorld resets state and seeds demand for all four formats so a
// multi-slot page can fill every slot. Placement keys are unique to this world
// (pgw-*) so they never collide with the demosite's seeded pl-sim-* inventory.
func BuildPagesWorld(t *testing.T, h *Harness) PagesWorld {
	t.Helper()
	h.Reset(t)
	h.ResetBillingLedger(t)

	w := PagesWorld{PlacementByFormat: map[pages.Format]string{}}
	w.Admin = h.CreateAdmin(t, "e2e-admin-pgw")
	w.PubAcc = h.CreatePublisher(t, "e2e-pub-pgw")
	w.Publisher = h.AddPublisher(t, w.PubAcc, "e2e-pub-pgw", "e2e-pgw.test")

	// One placement per format (low floors so the test bids clear easily).
	h.AddPlacement(t, w.Publisher, "pgw-display", 300, 250, 0.50, []string{"IAB12"})
	h.addNativePlacement(t, w.Publisher, "pgw-native", 0.50)
	h.AddVideoPlacement(t, w.Publisher, "pgw-video", 0.50, 5, 40)
	h.addAudioPlacement(t, w.Publisher, "pgw-audio", 0.50, 5, 40)
	w.PlacementByFormat[pages.Display] = "pgw-display"
	w.PlacementByFormat[pages.Native] = "pgw-native"
	w.PlacementByFormat[pages.Video] = "pgw-video"
	w.PlacementByFormat[pages.Audio] = "pgw-audio"

	// adv-acme is in the internal DSP allowlist (profiles/dsps/internal.yaml), so
	// its campaigns land in the DSP warm cache after RefreshAllCaches.
	w.AdvAcc = h.CreateAdvertiser(t, "adv-acme")
	h.GrantBalance(t, w.AdvAcc.ID, 100_000, "e2e-pgw-grant")
	io := h.CreateInsertionOrder(t, w.AdvAcc, "e2e-io-pgw", 5000)

	tgt := Targeting{Geos: []string{"USA"}, Devices: []string{"desktop"}}
	h.CreateCampaign(t, w.AdvAcc, io, "pgw-li-display", 6.0, 500, "pgw-cr-display", "adv-pgw.test", tgt)
	h.createNativeCampaign(t, w.AdvAcc, io, "pgw-li-native", 6.0, 500, "pgw-cr-native", "adv-pgw.test", tgt)
	h.CreateVideoCampaign(t, w.AdvAcc, io, "pgw-li-video", 6.0, 500, "pgw-cr-video", "adv-pgw.test", 15, tgt)
	h.createAudioCampaign(t, w.AdvAcc, io, "pgw-li-audio", 6.0, 500, "pgw-cr-audio", "adv-pgw.test", 15, tgt)

	h.RefreshAllCaches(t)
	return w
}

// SiteTenant is one seeded external-publisher site: the pages.Site definition,
// the publisher tenant it maps to, and the format→placement-key map for
// VisitPageWith.
type SiteTenant struct {
	Site              pages.Site
	Publisher         Publisher
	PlacementByFormat map[pages.Format]string
}

// MultiSiteWorld is several publisher tenants (one per pages.Site, each with a
// distinct revenue share) plus advertiser tenants bidding across all of them.
// Advertisers are split by format so each deterministically wins and books
// spend: acme = display+native, globex = video, initech = audio.
type MultiSiteWorld struct {
	Sites []SiteTenant
	// Representative campaign id per advertiser tenant, for per-advertiser
	// attribution assertions.
	AcmeDisplayCampaign  string
	AcmeNativeCampaign   string
	GlobexVideoCampaign  string
	InitechAudioCampaign string
}

// BuildMultiSiteWorld resets state and seeds the full "friends' websites" world:
// every pages.Site as its own publisher tenant (own placements + revenue share),
// and three advertiser tenants bidding across all of them. This is the fixture
// the multi-tenant e2e drives to prove per-publisher zero-slippage, reporting
// isolation, and correct per-publisher revenue split.
func BuildMultiSiteWorld(t *testing.T, h *Harness) MultiSiteWorld {
	t.Helper()
	h.Reset(t)
	h.ResetBillingLedger(t)

	admin := h.CreateAdmin(t, "e2e-admin-multisite")
	_ = admin

	var w MultiSiteWorld

	// One publisher tenant per site, with its own inventory + revenue share.
	for _, site := range pages.AllSites() {
		pubAcc := h.CreatePublisher(t, site.Publisher)
		pub := h.AddPublisher(t, pubAcc, site.Publisher, site.Domain)

		// Distinct revenue share: publisher keeps RevsharePct, platform fee is the
		// remainder. Proven per-tenant by the net_revenue/gross ratio in the test.
		feePct := 100 - site.RevsharePct
		h.SetPublisherContract(t, pub, "fixed",
			`{"revshare_model":"fixed","fee_pct":`+strconv.Itoa(feePct)+`}`)

		keys := site.PlacementByFormat()
		h.AddPlacement(t, pub, keys[pages.Display], 300, 250, 0.50, []string{"IAB12"})
		h.addNativePlacement(t, pub, keys[pages.Native], 0.50)
		h.AddVideoPlacement(t, pub, keys[pages.Video], 0.50, 5, 40)
		h.addAudioPlacement(t, pub, keys[pages.Audio], 0.50, 5, 40)

		w.Sites = append(w.Sites, SiteTenant{Site: site, Publisher: pub, PlacementByFormat: keys})
	}

	tgt := Targeting{Geos: []string{"USA"}, Devices: []string{"desktop"}}

	// Three advertiser tenants (all in the internal DSP allowlist), split by
	// format so each wins its own inventory rather than contending head-to-head
	// on one DSP. All fund large, all target USA/desktop.
	acme := h.CreateAdvertiser(t, "adv-acme")
	h.GrantBalance(t, acme.ID, 100_000, "e2e-multisite-acme")
	acmeIO := h.CreateInsertionOrder(t, acme, "e2e-io-acme", 10000)
	w.AcmeDisplayCampaign = h.CreateCampaign(t, acme, acmeIO, "ms-acme-display", 6.0, 2000, "ms-acme-display-cr", "acme.example", tgt).ID
	h.createNativeCampaign(t, acme, acmeIO, "ms-acme-native", 6.0, 2000, "ms-acme-native-cr", "acme.example", tgt)
	w.AcmeNativeCampaign = idgen.Derive("line_item", "ms-acme-native")

	globex := h.CreateAdvertiser(t, "adv-globex")
	h.GrantBalance(t, globex.ID, 100_000, "e2e-multisite-globex")
	globexIO := h.CreateInsertionOrder(t, globex, "e2e-io-globex", 10000)
	h.CreateVideoCampaign(t, globex, globexIO, "ms-globex-video", 6.0, 2000, "ms-globex-video-cr", "globex.example", 15, tgt)
	w.GlobexVideoCampaign = idgen.Derive("line_item", "ms-globex-video")

	initech := h.CreateAdvertiser(t, "adv-initech")
	h.GrantBalance(t, initech.ID, 100_000, "e2e-multisite-initech")
	initechIO := h.CreateInsertionOrder(t, initech, "e2e-io-initech", 10000)
	h.createAudioCampaign(t, initech, initechIO, "ms-initech-audio", 6.0, 2000, "ms-initech-audio-cr", "initech.example", 15, tgt)
	w.InitechAudioCampaign = idgen.Derive("line_item", "ms-initech-audio")

	h.RefreshAllCaches(t)
	return w
}

// addNativePlacement inserts a native placement (no fixed box).
func (h *Harness) addNativePlacement(t *testing.T, pub Publisher, externalKey string, floor float64) {
	t.Helper()
	id := idgen.Derive("placement", externalKey)
	h.WithTenant(t, pub.AccountID, func(tx *sql.Tx) {
		const q = `
INSERT INTO placements (id, publisher_id, account_id, name, format, floor_price, floor_currency, page_url_pattern, status, created_at, updated_at)
VALUES ($1, $2, $3, $4, 'native', $5, 'USD', $6, 'active', now(), now())
ON CONFLICT (id) DO UPDATE SET floor_price = EXCLUDED.floor_price, format = 'native', updated_at = now()`
		pageURL := "https://" + pub.Domain + "/n/" + externalKey
		if _, err := tx.Exec(q, id, pub.ID, pub.AccountID, externalKey, floor, pageURL); err != nil {
			t.Fatalf("native placements insert: %v", err)
		}
	})
}

// addAudioPlacement inserts an audio placement carrying a duration window
// (audio_config mirrors video_config's [min,max] gate in the DSP).
func (h *Harness) addAudioPlacement(t *testing.T, pub Publisher, externalKey string, floor float64, minDur, maxDur int) {
	t.Helper()
	id := idgen.Derive("placement", externalKey)
	audioJSON, _ := json.Marshal(map[string]any{
		"min_duration": minDur, "max_duration": maxDur, "mimes": []string{"audio/mpeg"},
	})
	h.WithTenant(t, pub.AccountID, func(tx *sql.Tx) {
		const q = `
INSERT INTO placements (id, publisher_id, account_id, name, format, floor_price, floor_currency, page_url_pattern, status, video_config, created_at, updated_at)
VALUES ($1, $2, $3, $4, 'audio', $5, 'USD', $6, 'active', $7, now(), now())
ON CONFLICT (id) DO UPDATE SET floor_price = EXCLUDED.floor_price, format = 'audio', video_config = EXCLUDED.video_config, updated_at = now()`
		pageURL := "https://" + pub.Domain + "/a/" + externalKey
		if _, err := tx.Exec(q, id, pub.ID, pub.AccountID, externalKey, floor, pageURL, audioJSON); err != nil {
			t.Fatalf("audio placements insert: %v", err)
		}
	})
}

// createNativeCampaign seeds a live native line item + a native creative whose
// asset set carries a title (the DSP's native match gate).
func (h *Harness) createNativeCampaign(t *testing.T, owner Account, io InsertionOrder, externalKey string, baseBid, dailyBudget float64, creativeExternalKey, creativeDomain string, targeting Targeting) {
	t.Helper()
	lineItemID := idgen.Derive("line_item", externalKey)
	creativeID := idgen.Derive("creative", creativeExternalKey)
	targetingID := idgen.Derive("targeting", externalKey)
	assets, _ := json.Marshal(map[string]any{
		"title": "E2E Native Ad", "main_image": "https://cdn." + creativeDomain + "/n.jpg",
		"main_image_w": 1200, "main_image_h": 627, "sponsored": "E2E", "body": "Native body",
		"cta": "Learn more", "landing_url": "https://" + creativeDomain,
	})
	h.WithTenant(t, owner.ID, func(tx *sql.Tx) {
		const liQ = `
INSERT INTO line_items (id, account_id, insertion_order_id, name, status, format, bid_strategy, base_bid, bid_currency, daily_budget, pacing_mode, shading_mode, creative_rotation, timezone, created_at, updated_at)
VALUES ($1, $2, $3, $4, 'live', 'native', 'cpm', $5, 'USD', $6, 'asap', 'disabled', 'bandit', 'UTC', now(), now())
ON CONFLICT (id) DO UPDATE SET status = 'live', format = 'native', base_bid = EXCLUDED.base_bid, daily_budget = EXCLUDED.daily_budget, updated_at = now()`
		if _, err := tx.Exec(liQ, lineItemID, owner.ID, io.ID, externalKey, baseBid, dailyBudget); err != nil {
			t.Fatalf("native line_items insert: %v", err)
		}
		const trQ = `
INSERT INTO targeting_rules (id, line_item_id, account_id, include_geo, include_device, bid_modifiers, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, '{}', now(), now())
ON CONFLICT (line_item_id) DO UPDATE SET include_geo = EXCLUDED.include_geo, include_device = EXCLUDED.include_device, updated_at = now()`
		if _, err := tx.Exec(trQ, targetingID, lineItemID, owner.ID, pq.StringArray(targeting.Geos), pq.StringArray(targeting.Devices)); err != nil {
			t.Fatalf("native targeting_rules insert: %v", err)
		}
		const crQ = `
INSERT INTO creatives (id, account_id, name, format, landing_url, native_assets, review_status, created_at, updated_at)
VALUES ($1, $2, $3, 'native', $4, $5, 'approved', now(), now())
ON CONFLICT (id) DO UPDATE SET native_assets = EXCLUDED.native_assets, format = 'native', updated_at = now()`
		if _, err := tx.Exec(crQ, creativeID, owner.ID, creativeExternalKey, "https://"+creativeDomain, assets); err != nil {
			t.Fatalf("native creatives insert: %v", err)
		}
		const linkQ = `
INSERT INTO line_item_creatives (line_item_id, creative_id, weight) VALUES ($1, $2, 100)
ON CONFLICT (line_item_id, creative_id) DO NOTHING`
		if _, err := tx.Exec(linkQ, lineItemID, creativeID); err != nil {
			t.Fatalf("native line_item_creatives insert: %v", err)
		}
	})
}

// createAudioCampaign seeds a live audio line item + an audio creative
// (format='audio', asset_url + duration within the placement window).
func (h *Harness) createAudioCampaign(t *testing.T, owner Account, io InsertionOrder, externalKey string, baseBid, dailyBudget float64, creativeExternalKey, creativeDomain string, durationSec int, targeting Targeting) {
	t.Helper()
	lineItemID := idgen.Derive("line_item", externalKey)
	creativeID := idgen.Derive("creative", creativeExternalKey)
	targetingID := idgen.Derive("targeting", externalKey)
	h.WithTenant(t, owner.ID, func(tx *sql.Tx) {
		const liQ = `
INSERT INTO line_items (id, account_id, insertion_order_id, name, status, format, bid_strategy, base_bid, bid_currency, daily_budget, pacing_mode, shading_mode, creative_rotation, timezone, created_at, updated_at)
VALUES ($1, $2, $3, $4, 'live', 'audio', 'cpm', $5, 'USD', $6, 'asap', 'disabled', 'bandit', 'UTC', now(), now())
ON CONFLICT (id) DO UPDATE SET status = 'live', format = 'audio', base_bid = EXCLUDED.base_bid, daily_budget = EXCLUDED.daily_budget, updated_at = now()`
		if _, err := tx.Exec(liQ, lineItemID, owner.ID, io.ID, externalKey, baseBid, dailyBudget); err != nil {
			t.Fatalf("audio line_items insert: %v", err)
		}
		const trQ = `
INSERT INTO targeting_rules (id, line_item_id, account_id, include_geo, include_device, bid_modifiers, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, '{}', now(), now())
ON CONFLICT (line_item_id) DO UPDATE SET include_geo = EXCLUDED.include_geo, include_device = EXCLUDED.include_device, updated_at = now()`
		if _, err := tx.Exec(trQ, targetingID, lineItemID, owner.ID, pq.StringArray(targeting.Geos), pq.StringArray(targeting.Devices)); err != nil {
			t.Fatalf("audio targeting_rules insert: %v", err)
		}
		const crQ = `
INSERT INTO creatives (id, account_id, name, format, duration_seconds, asset_url, landing_url, review_status, created_at, updated_at)
VALUES ($1, $2, $3, 'audio', $4, $5, $6, 'approved', now(), now())
ON CONFLICT (id) DO UPDATE SET asset_url = EXCLUDED.asset_url, duration_seconds = EXCLUDED.duration_seconds, format = 'audio', updated_at = now()`
		assetURL := "https://cdn." + creativeDomain + "/" + creativeExternalKey + ".mp3"
		if _, err := tx.Exec(crQ, creativeID, owner.ID, creativeExternalKey, durationSec, assetURL, "https://"+creativeDomain); err != nil {
			t.Fatalf("audio creatives insert: %v", err)
		}
		const linkQ = `
INSERT INTO line_item_creatives (line_item_id, creative_id, weight) VALUES ($1, $2, 100)
ON CONFLICT (line_item_id, creative_id) DO NOTHING`
		if _, err := tx.Exec(linkQ, lineItemID, creativeID); err != nil {
			t.Fatalf("audio line_item_creatives insert: %v", err)
		}
	})
}
