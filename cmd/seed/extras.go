package main

import (
	"context"
	"fmt"

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
