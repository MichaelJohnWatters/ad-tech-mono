package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/idgen"
	"github.com/lib/pq"
	"golang.org/x/crypto/bcrypt"
)

// SeedFeatureBaseline seeds the recent-feature entities the original
// profiles never covered. Found the hard way after a factory reset
// (2026-07-26): features that were only ever "live-verified" by hand —
// data providers, agency links, labeled audience segments, exchange
// rates, house ads — came back EMPTY on a fresh stack, leaving portals
// hollow and one billing e2e permanently skipped. Everything here is
// idempotent (deterministic IDs + upserts) like the rest of the seed.
func (in *inserter) SeedFeatureBaseline(ctx context.Context) error {
	if err := in.seedDataProviders(ctx); err != nil {
		return err
	}
	if err := in.seedAudienceSegments(ctx); err != nil {
		return err
	}
	if err := in.seedThemedSegments(ctx); err != nil {
		return err
	}
	if err := in.seedProductCatalog(ctx); err != nil {
		return err
	}
	if err := in.seedConversionConfigs(ctx); err != nil {
		return err
	}
	if err := in.seedMarketplaceListings(ctx); err != nil {
		return err
	}
	if err := in.seedAgency(ctx); err != nil {
		return err
	}
	if err := in.seedExchangeRates(ctx); err != nil {
		return err
	}
	if err := in.seedHouseAds(ctx); err != nil {
		return err
	}
	return nil
}

// seedDataProviders creates two DMP provider entities (ADR 0009) on the
// primary dev advertiser: a relaxed first-party CRM feed, and a licensed
// third-party provider with encryption_expected=true — the pair the
// data-provider e2e uses to prove provenance stamping and the
// cleartext-rejection gate.
func (in *inserter) seedDataProviders(ctx context.Context) error {
	account := idgen.Derive("account", "adv-globex")
	providers := []struct {
		key, name, kind, party, licence, idType string
		encrypted                               bool
		costCPMMicros                           int64
	}{
		{"dp-globex-crm", "Globex CRM Feed", "crm", "first", "first_party", "email_sha256", false, 0},
		{"dp-liveramp", "LiveRamp Licensed Segments", "dmp", "third", "purchased", "uid2", true, 250_000},
	}
	for _, p := range providers {
		var cost any
		if p.costCPMMicros > 0 {
			cost = p.costCPMMicros
		}
		if _, err := in.db.ExecContext(ctx, `
INSERT INTO data_providers (id, account_id, name, kind, default_party, default_licence, default_id_type, encryption_expected, cost_cpm_micros, status, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'active', now(), now())
ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, kind = EXCLUDED.kind,
  default_party = EXCLUDED.default_party, default_licence = EXCLUDED.default_licence,
  default_id_type = EXCLUDED.default_id_type, encryption_expected = EXCLUDED.encryption_expected,
  cost_cpm_micros = EXCLUDED.cost_cpm_micros, status = 'active', updated_at = now()`,
			idgen.Derive("data_provider", p.key), account, p.name, p.kind, p.party, p.licence, p.idType, p.encrypted, cost); err != nil {
			return fmt.Errorf("seed data provider %s: %w", p.key, err)
		}
	}
	in.log.Info("seeded data providers", "count", len(providers))
	return nil
}

// seedAudienceSegments gives the primary advertiser a few public segments
// carrying IAB taxonomy labels (so segtax + data-fee demos light up) and a
// handful of members matching the simulator's low-numbered user ids.
func (in *inserter) seedAudienceSegments(ctx context.Context) error {
	account := idgen.Derive("account", "adv-globex")
	segments := []struct {
		key, name string
		taxonomy  []string // ids present in the seeded iab_audience_taxonomy subset
	}{
		{"seg-young-adults", "Young Adults 18-34", []string{"3", "4"}},
		{"seg-demo-interest", "Demo Interest Buyers", []string{"1"}},
		{"seg-suppression", "Opt-Out Suppression", nil},
	}
	for _, s := range segments {
		segID := idgen.Derive("segment", s.key)
		suppression := s.taxonomy == nil
		segType := "first_party"
		if suppression {
			segType = "suppression"
		}
		if _, err := in.db.ExecContext(ctx, `
INSERT INTO audience_segments (id, account_id, name, type, size_estimate, suppression, taxonomy_categories, status, source, created_at, updated_at)
VALUES ($1, $2, $3, $6, 5, $4, $5, 'active', 'seed', now(), now())
ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, taxonomy_categories = EXCLUDED.taxonomy_categories, status = 'active', updated_at = now()`,
			segID, account, s.name, suppression, pq.Array(s.taxonomy), segType); err != nil {
			return fmt.Errorf("seed segment %s: %w", s.key, err)
		}
		for i := 1; i <= 5; i++ {
			userID := fmt.Sprintf("user-%03d", i)
			if _, err := in.db.ExecContext(ctx, `
INSERT INTO audience_segment_members (segment_id, user_id, account_id, added_at)
VALUES ($1, $2, $3, now()) ON CONFLICT (segment_id, user_id) DO NOTHING`,
				segID, userID, account); err != nil {
				return fmt.Errorf("seed segment member %s/%s: %w", s.key, userID, err)
			}
		}
	}
	in.log.Info("seeded audience segments", "count", len(segments), "members_per", 5)
	return nil
}

// seedThemedSegments builds the READABLE core of the themed two-tier world
// (operator design decision, 2026-08-07): behavioural rule segments whose
// membership is EARNED by the themed simulator personas browsing the themed
// publishers (profiles/publishers/themed.yaml) — the hourly profile-builder
// evaluates the category rules over behaviour_signals and enrolls them. The
// themed campaigns in profiles/dsps/internal.yaml target these segments, so
// verification reads like English: the Ford ad wins for truck intenders once
// (and only once) they've browsed enough matching pages.
//
// The owning accounts are created by the campaign pass (upsertAccounts runs
// before SeedFeatureBaseline), so this only has to attach segments — plus
// friendly display names, because "Advertiser adv-barkbox" defeats the
// memorable-world purpose.
func (in *inserter) seedThemedSegments(ctx context.Context) error {
	names := map[string]string{
		"adv-barkbox":   "Ford Motors",
		"adv-whiskerco": "Whisker & Co Cat Treats",
		"adv-beanbarn":  "Bean Barn Roasters",
		"adv-sweatlabs": "Sweat Labs Nutrition",
	}
	for ext, name := range names {
		if _, err := in.db.ExecContext(ctx,
			`UPDATE accounts SET name = $2, updated_at = now() WHERE id = $1`,
			idgen.Derive("account", ext), name); err != nil {
			return fmt.Errorf("themed account name %s: %w", ext, err)
		}
	}

	dogLovers := idgen.Derive("segment", "seg-dog-lovers")
	coffeeBrowsers := idgen.Derive("segment", "seg-coffee-browsers")
	segments := []struct {
		key, account, name, segType, visibility, rule string
	}{
		// The four flagship interest segments: ≥3 (or ≥2) consented visits
		// to matching-category pages inside 30 days.
		{"seg-dog-lovers", "adv-barkbox", "Truck Intenders", "behavioral", "dsp_private",
			`{"event":"request","category":"dogs","min_count":3,"window_days":30}`},
		{"seg-cat-lovers", "adv-whiskerco", "Cat Lovers", "behavioral", "dsp_private",
			`{"event":"request","category":"cats","min_count":3,"window_days":30}`},
		// Public on purpose: the one themed segment that exercises the SSP's
		// public-visibility stamp path (user.ext.segments to bidders).
		{"seg-coffee-snobs", "adv-beanbarn", "Coffee Snobs", "behavioral", "public",
			`{"event":"request","category":"coffee","min_count":3,"window_days":30}`},
		{"seg-fitness-fans", "adv-sweatlabs", "Fitness Fans", "behavioral", "dsp_private",
			`{"event":"request","category":"fitness","min_count":2,"window_days":30}`},
		// Ford's own commuter-interest rule, existing only so the composite
		// below can reference same-account segments (composite rules are
		// account-scoped).
		{"seg-coffee-browsers", "adv-barkbox", "Commuters (Ford)", "behavioral", "dsp_private",
			`{"event":"request","category":"coffee","min_count":2,"window_days":30}`},
		// Derived kinds, one each, so every rule shape has a readable demo:
		// composite (truck intenders who also commute — the weekend-adventurer
		// persona) and lookalike (seeded from truck intenders).
		{"seg-dog-cafe-crowd", "adv-barkbox", "Weekend Adventurers", "composite", "dsp_private",
			fmt.Sprintf(`{"kind":"composite","all_of":[%q,%q]}`, dogLovers, coffeeBrowsers)},
		{"seg-dog-lookalikes", "adv-barkbox", "Truck Intender Lookalikes", "lookalike", "dsp_private",
			fmt.Sprintf(`{"kind":"lookalike","seed_segment":%q,"min_similarity":0.5}`, dogLovers)},
		// The fast-path demo: audience-rt enrolls a /v1/t/rt?tag=dogfood-cart
		// visitor within seconds (min_count 1 = the real-time boundary).
		{"seg-dogfood-cart", "adv-barkbox", "Ford Cart Abandoners", "retargeting", "dsp_private",
			`{"event":"site_visit","tag":"dogfood-cart","min_count":1,"window_days":30}`},
		// Same rule, tag "checkout" — cmd/demoadv's checkout page fires that
		// tag out of the box, so running the demo shop as this advertiser
		// (DEMOADV_ACCOUNT_ID=adv-barkbox's uuid) enrolls real browser
		// visitors with zero pixel configuration. The chase campaign
		// (li-dogfood-cart-rt) targets both cart segments.
		{"seg-shop-checkout", "adv-barkbox", "Shop Checkout Abandoners", "retargeting", "dsp_private",
			`{"event":"site_visit","tag":"checkout","min_count":1,"window_days":30}`},
		// The demo shop's PRODUCT page (/models/f150) fires tag "f150-interest"
		// out of the box, so a single product-page view ENROLLS the visitor in
		// real time (min_count 1 = the audience-rt fast path) — the clearest
		// earned-audience beat: browse one product → retargetable in seconds.
		// The chase campaign (li-dogfood-cart-rt) targets this segment too.
		{"seg-shop-product", "adv-barkbox", "Shop Product Viewers", "retargeting", "dsp_private",
			`{"event":"site_visit","tag":"f150-interest","min_count":1,"window_days":30}`},
	}
	for _, s := range segments {
		if _, err := in.db.ExecContext(ctx, `
INSERT INTO audience_segments (id, account_id, name, type, visibility, rule, status, source, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6::jsonb, 'active', 'seed', now(), now())
ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, type = EXCLUDED.type,
  visibility = EXCLUDED.visibility, rule = EXCLUDED.rule, status = 'active', updated_at = now()`,
			idgen.Derive("segment", s.key), idgen.Derive("account", s.account),
			s.name, s.segType, s.visibility, s.rule); err != nil {
			return fmt.Errorf("seed themed segment %s: %w", s.key, err)
		}
	}
	in.log.Info("seeded themed segments", "count", len(segments))
	return nil
}

// seedProductCatalog gives Ford Motors (adv-barkbox — the demoadv shop
// advertiser) a product catalog so the Dynamic Product Ads path has something
// to render (slice 3) and suppress by SKU (slice 4) out of the box. SKUs match
// the demoadv shop grid; prices are micro-dollars (money convention). Idempotent
// upsert by (account_id, sku), same as the ingest path writes.
func (in *inserter) seedProductCatalog(ctx context.Context) error {
	account := idgen.Derive("account", "adv-barkbox")
	products := []struct {
		sku, title, category string
		priceMicros          int64
		complementSKU        string
	}{
		{"FORD-F150", "F-150", "trucks", 38_000_000_000, "FORD-MAVERICK"},
		{"FORD-MUSTANG", "Mustang", "cars", 32_000_000_000, "FORD-MACHE"},
		{"FORD-EXPLORER", "Explorer", "suv", 42_000_000_000, "FORD-BRONCO"},
		{"FORD-BRONCO", "Bronco", "suv", 40_000_000_000, "FORD-EXPLORER"},
		{"FORD-ESCAPE", "Escape", "suv", 30_000_000_000, "FORD-MAVERICK"},
		{"FORD-MAVERICK", "Maverick", "trucks", 26_000_000_000, "FORD-F150"},
		{"FORD-MACHE", "Mustang Mach-E", "ev", 45_000_000_000, "FORD-MUSTANG"},
	}
	for _, p := range products {
		// The AUTO theme SVG (auto-300x250.svg) — catalog-shaped placeholder art
		// for the Ford models. Must be an ABSOLUTE, browser-reachable URL: the DPA
		// renders this <img> on the PUBLISHER's origin (cross-site chase), so a
		// relative "themes/…" path would resolve against the publisher host and
		// 404 (broken-image icon). Prefix with the same creatives base every other
		// creative asset uses.
		imageURL := strings.TrimRight(in.creativeAssetBase, "/") + "/" + creativeAssetByTheme("auto", 300, 250)
		shopBase := in.shopURLBase
		if shopBase == "" {
			shopBase = "http://localhost:9200"
		}
		productURL := strings.TrimRight(shopBase, "/") + "/models/" + p.sku
		if _, err := in.db.ExecContext(ctx, `
INSERT INTO products (account_id, sku, title, description, image_url, price_micros, currency, availability, product_url, category, complement_sku, source, origin_trace, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, 'USD', 'in_stock', $7, $8, $9, 'seed', '', now(), now())
ON CONFLICT (account_id, sku) DO UPDATE SET
  title = EXCLUDED.title, image_url = EXCLUDED.image_url, price_micros = EXCLUDED.price_micros,
  product_url = EXCLUDED.product_url, category = EXCLUDED.category,
  complement_sku = EXCLUDED.complement_sku, updated_at = now()`,
			account, p.sku, p.title, "The all-new Ford "+p.title+" — built Ford tough.",
			imageURL, p.priceMicros, productURL, p.category, p.complementSKU); err != nil {
			return fmt.Errorf("seed product %s: %w", p.sku, err)
		}
	}
	in.log.Info("seeded product catalog", "account", "adv-barkbox", "products", len(products))
	return nil
}

// seedConversionConfigs gives the demo advertisers a named conversion EVENT
// definition (migration 048) each, so the advertiser portal's Conversions tab
// shows a "Purchase" event + its embed pixel out of the box instead of a bare
// "no events defined" table. These are just the setup-layer definitions; the
// actual recorded conversions come from the tracker /v1/t/conv path. Idempotent
// (unique on account_id+name). Seeded as the owner role, which bypasses the RLS
// policy, so cross-tenant inserts work like the rest of SeedFeatureBaseline.
func (in *inserter) seedConversionConfigs(ctx context.Context) error {
	configs := []struct {
		account, name, eventType string
		defaultValue             float64
	}{
		{"adv-barkbox", "Vehicle Purchase", "purchase", 38000}, // Ford Motors — the shop/DPA demo
		{"adv-barkbox", "Request a Quote", "lead", 0},
		{"adv-globex", "Purchase", "purchase", 50},
		{"adv-acme", "Purchase", "purchase", 50},
		{"adv-initech", "Free Trial Signup", "signup", 0},
	}
	for _, c := range configs {
		if _, err := in.db.ExecContext(ctx, `
INSERT INTO conversion_configs (account_id, name, event_type, default_value, currency, status, created_at, updated_at)
VALUES ($1, $2, $3, $4, 'USD', 'active', now(), now())
ON CONFLICT (account_id, name) DO UPDATE SET
  event_type = EXCLUDED.event_type, default_value = EXCLUDED.default_value, status = 'active', updated_at = now()`,
			idgen.Derive("account", c.account), c.name, c.eventType, c.defaultValue); err != nil {
			return fmt.Errorf("seed conversion config %s/%s: %w", c.account, c.name, err)
		}
	}
	in.log.Info("seeded conversion configs", "count", len(configs))
	return nil
}

// seedMarketplaceListings publishes a couple of PUBLIC seeded segments to the
// data marketplace (PLAN Phase 10) so the catalog isn't empty on a fresh stack:
// Bean Barn's public "Coffee Snobs" (a themed behavioural segment) and Globex's
// "Young Adults 18-34". Idempotent (upsert keyed on segment_id). size_estimate
// snapshots the segment's live member count.
func (in *inserter) seedMarketplaceListings(ctx context.Context) error {
	listings := []struct {
		segKey, account, name, description string
		cpmMicros                          int64
	}{
		{"seg-coffee-snobs", "adv-beanbarn", "UK Coffee Enthusiasts",
			"Consented readers who regularly browse coffee content — high purchase intent for premium beans + brewing gear.", 500_000},
		{"seg-young-adults", "adv-globex", "Young Adults 18-34",
			"A broad consented young-adult cohort for reach + brand campaigns.", 750_000},
	}
	for _, l := range listings {
		account := idgen.Derive("account", l.account)
		segID := idgen.Derive("segment", l.segKey)
		if _, err := in.db.ExecContext(ctx, `
INSERT INTO marketplace_listings (account_id, segment_id, name, description, size_estimate, cpm_surcharge_micros, preview, status, created_at, updated_at)
SELECT $1, $2, $3, $4, COALESCE((SELECT count(*) FROM audience_segment_members WHERE segment_id = $2), 0),
       $5, '{}'::jsonb, 'active', now(), now()
WHERE EXISTS (SELECT 1 FROM audience_segments WHERE id = $2 AND account_id = $1 AND visibility = 'public')
ON CONFLICT (segment_id) DO UPDATE SET
  name = EXCLUDED.name, description = EXCLUDED.description,
  cpm_surcharge_micros = EXCLUDED.cpm_surcharge_micros, status = 'active', updated_at = now()`,
			account, segID, l.name, l.description, l.cpmMicros); err != nil {
			return fmt.Errorf("seed marketplace listing %s: %w", l.segKey, err)
		}
	}
	in.log.Info("seeded marketplace listings", "count", len(listings))
	return nil
}

// seedAgency creates the dev agency account, links it to the two primary
// advertisers (act-as via X-Act-As-Account), and gives it a dev login so
// the portal switcher has something to switch between on a fresh stack.
func (in *inserter) seedAgency(ctx context.Context) error {
	agencyID := idgen.Derive("account", "agency-omnicorp")
	if _, err := in.db.ExecContext(ctx, `
INSERT INTO accounts (id, name, email, type, status, created_at, updated_at)
VALUES ($1, 'Omnicorp Media Agency', 'agency@adtech.local', 'agency', 'active', now(), now())
ON CONFLICT (id) DO UPDATE SET status = 'active', updated_at = now()`, agencyID); err != nil {
		return fmt.Errorf("seed agency account: %w", err)
	}
	for _, managedKey := range []string{"adv-globex", "adv-acme"} {
		managedID := idgen.Derive("account", managedKey)
		var exists bool
		if err := in.db.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM accounts WHERE id = $1)`, managedID).Scan(&exists); err != nil {
			return fmt.Errorf("check managed account %s: %w", managedKey, err)
		}
		if !exists {
			continue
		}
		if _, err := in.db.ExecContext(ctx, `
INSERT INTO agency_managed_accounts (agency_account_id, managed_account_id, created_at)
VALUES ($1, $2, now()) ON CONFLICT DO NOTHING`, agencyID, managedID); err != nil {
			return fmt.Errorf("seed agency link %s: %w", managedKey, err)
		}
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(DevAdminPassword), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash agency password: %w", err)
	}
	if _, err := in.db.ExecContext(ctx, `
INSERT INTO team_members (id, account_id, email, name, role, password_hash, status, created_at, updated_at)
VALUES ($1, $2, 'agency@adtech.local', 'Dev Agency', 'owner', $3, 'active', now(), now())
ON CONFLICT (id) DO UPDATE SET password_hash = EXCLUDED.password_hash, status = 'active', updated_at = now()`,
		idgen.Derive("user", "dev-agency"), agencyID, string(hash)); err != nil {
		return fmt.Errorf("seed agency login: %w", err)
	}
	in.log.Info("seeded agency + managed links", "email", "agency@adtech.local")
	return nil
}

// seedExchangeRates writes a baseline USD rate row per major currency for
// today — the seed the currency-conversion billing e2e has been skipped on.
func (in *inserter) seedExchangeRates(ctx context.Context) error {
	rates := map[string]float64{"EUR": 0.92, "GBP": 0.79, "JPY": 148.0, "CAD": 1.36}
	for cur, rate := range rates {
		if _, err := in.db.ExecContext(ctx, `
INSERT INTO exchange_rates (base_currency, target_currency, rate, effective_date, source)
VALUES ('USD', $1, $2, CURRENT_DATE, 'seed')
ON CONFLICT (base_currency, target_currency, effective_date) DO UPDATE SET rate = EXCLUDED.rate`,
			cur, rate); err != nil {
			return fmt.Errorf("seed exchange rate %s: %w", cur, err)
		}
	}
	in.log.Info("seeded exchange rates", "currencies", len(rates))
	return nil
}

// seedHouseAds gives the publisher ad server one enabled display fallback
// so a no-fill on a fresh stack renders a house ad instead of a blank slot.
func (in *inserter) seedHouseAds(ctx context.Context) error {
	if _, err := in.db.ExecContext(ctx, `
INSERT INTO house_ads (id, format, name, markup, landing_url, enabled, weight, created_at, updated_at)
VALUES ($1, 'display', 'House: Advertise With Us',
  '<div style="width:100%;height:100%;display:flex;align-items:center;justify-content:center;background:#0f172a;color:#e2e8f0;font-family:sans-serif;font-size:14px">Advertise with us — adtech.local</div>',
  'https://gateway.adtech.local/', true, 1, now(), now())
ON CONFLICT (id) DO UPDATE SET enabled = true, updated_at = now()`,
		idgen.Derive("house_ad", "house-display-1")); err != nil {
		return fmt.Errorf("seed house ad: %w", err)
	}
	in.log.Info("seeded house ad", "format", "display")
	return nil
}
