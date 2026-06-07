package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/lib/pq"
)

// inserter writes the resolved YAML structure into Postgres.
//
// Every row uses a deterministic UUID derived from its external string key
// (DeriveID(kind, key)) so re-running the seed UPSERTs in place. RLS is
// scoped per-account via SET LOCAL inside each transaction — accounts and
// insertion orders go in first under each tenant before line items reference them.
type inserter struct {
	db  *sql.DB
	log *slog.Logger
	// creativeAssetBase is the browser-reachable URL prefix for image
	// creative assets stored in Minio. Used for the asset_url half of
	// the split (see creative_assets.go). e.g.
	// "http://localhost:8080/v1/creatives" → full URL becomes
	// "{base}/themes/{theme}-300x250.svg". Empty = no asset path,
	// every creative falls back to inline HTML (degraded but
	// functional in offline / no-Minio dev).
	creativeAssetBase string
	// landingURLBase is the browser-reachable URL prefix for the demo
	// landing pages served by the gateway at /dev/landing/{slug}.
	// landing_url for each seeded creative becomes "{base}/{brand-slug}"
	// so clicks redirect into our own gateway instead of bouncing off
	// the squatter who happens to own e.g. luxauto.com. Empty falls
	// back to https://{creative_domain} (legacy behaviour) so the seed
	// still works in environments without a gateway.
	landingURLBase string
}

// brandSlugFromDomain reduces "acme-shoes.com" → "acme-shoes" and
// "globex-tech.com" → "globex-tech" so the landing-page URL is stable
// across the YAML's creative_domain and the landingThemes table in
// cmd/gateway/landing.go.
func brandSlugFromDomain(domain string) string {
	if domain == "" {
		return "default"
	}
	s := domain
	if i := strings.IndexByte(s, '.'); i > 0 {
		s = s[:i]
	}
	return strings.ToLower(s)
}

// materialiseCreatives expands a CampaignConfig into the list of
// CreativeYAMLs the inserter writes. The new YAML shape is a Creatives
// array (one entry per size variant). The legacy shape is a single
// creative_id + creative_domain pair at the line-item level — we
// synthesise a one-element 300x250 list from those so old YAMLs still
// produce valid rows. If both shapes appear on the same campaign, the
// explicit Creatives list wins (the legacy fields are ignored).
func materialiseCreatives(c CampaignConfig) []CreativeYAML {
	if len(c.Creatives) > 0 {
		out := make([]CreativeYAML, 0, len(c.Creatives))
		for _, cv := range c.Creatives {
			w, h := cv.Width, cv.Height
			if w == 0 || h == 0 {
				w, h = 300, 250
			}
			id := cv.ID
			if id == "" {
				id = fmt.Sprintf("%s-%dx%d", c.ID, w, h)
			}
			domain := cv.Domain
			if domain == "" {
				domain = c.CreativeDomain
			}
			out = append(out, CreativeYAML{
				ID: id, Width: w, Height: h, Domain: domain,
				Format: cv.Format, MediaURL: cv.MediaURL, Duration: cv.Duration,
			})
		}
		return out
	}
	if c.CreativeID == "" {
		return nil
	}
	return []CreativeYAML{{
		ID:     c.CreativeID,
		Width:  300,
		Height: 250,
		Domain: c.CreativeDomain,
	}}
}

// landingURLFor returns the landing URL for a creative. When base is
// set (the common case in dev — gateway hosts /dev/landing/{slug}) the
// URL routes back into our own gateway so clicks land on a themed
// mock page that shows the trace_id. When base is empty (offline /
// no-gateway setup) it falls back to https://{creative_domain} so the
// seed still produces something tracker-redirectable.
func landingURLFor(base, domain string) string {
	if base != "" {
		return strings.TrimSuffix(base, "/") + "/" + brandSlugFromDomain(domain)
	}
	if domain == "" {
		return "https://example.com"
	}
	return "https://" + domain
}

// SeedAll runs the full seed for a parsed set of DSP profiles. Idempotent.
func (in *inserter) SeedAll(ctx context.Context, profiles []DSPProfile) error {
	// Pass 0: upsert each YAML profile as a dsps row. Each row is the
	// runtime identity of one DSP pod: name + noise/no_bid behavior. The
	// returned name→id map lets us link advertiser accounts to their
	// managing DSP in the next pass.
	dspIDsByName, err := in.upsertDSPs(ctx, profiles)
	if err != nil {
		return fmt.Errorf("dsps: %w", err)
	}

	// Pass 1: gather unique accounts + IOs across all profiles so each tenant
	// row is inserted once before any line items reference it. RLS requires
	// the account row to exist before SET LOCAL can be checked against it.
	// Also build accountToDSP so accounts are stamped with the DSP that
	// manages them (their YAML's owning profile).
	accounts := map[string]string{}    // externalID → display name
	accountToDSP := map[string]string{} // externalID → dsp UUID
	ios := map[string]ioInsertPayload{}

	for _, p := range profiles {
		dspID := dspIDsByName[p.Name]
		for _, c := range p.Campaigns {
			if c.AccountID != "" {
				accounts[c.AccountID] = displayName(c.AccountID, "Advertiser")
				// If an account appears in multiple profiles, last write wins.
				// In practice each advertiser belongs to one DSP — this just
				// makes the ambiguity (if it existed) deterministic.
				accountToDSP[c.AccountID] = dspID
			}
			if c.IOId != "" {
				ios[c.IOId] = ioInsertPayload{
					external:  c.IOId,
					accountID: DeriveID("account", c.AccountID),
					name:      displayName(c.IOId, "Insertion Order"),
					currency:  defaultStr(c.Currency, "USD"),
				}
			}
		}
	}

	if err := in.upsertAccounts(ctx, accounts, accountToDSP); err != nil {
		return fmt.Errorf("accounts: %w", err)
	}
	if err := in.upsertInsertionOrders(ctx, ios); err != nil {
		return fmt.Errorf("insertion_orders: %w", err)
	}

	// Pass 2: campaigns + targeting + creatives, tenant-scoped per row.
	for _, p := range profiles {
		for _, c := range p.Campaigns {
			if err := in.upsertCampaign(ctx, c); err != nil {
				return fmt.Errorf("line_item %s: %w", c.ID, err)
			}
		}
	}
	return nil
}

func (in *inserter) upsertAccounts(ctx context.Context, accounts map[string]string, accountToDSP map[string]string) error {
	// dsp_id is nullable on accounts (publishers/admins/staff don't have one)
	// so we accept an empty string and pass NULL when no DSP owns this account.
	const q = `
INSERT INTO accounts (id, name, email, type, currency, status, dsp_id, created_at, updated_at)
VALUES ($1, $2, $3, 'advertiser', 'USD', 'active', NULLIF($4, '')::uuid, now(), now())
ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, dsp_id = EXCLUDED.dsp_id, updated_at = now()`
	for ext, name := range accounts {
		id := DeriveID("account", ext)
		email := ext + "@seed.local"
		dspID := accountToDSP[ext] // empty string → NULL via NULLIF
		if _, err := in.db.ExecContext(ctx, q, id, name, email, dspID); err != nil {
			return fmt.Errorf("upsert account %s: %w", ext, err)
		}
		in.log.Debug("seeded account", "external", ext, "id", id, "dsp_id", dspID)
	}
	return nil
}

// upsertDSPs writes one dsps row per YAML profile. Returns name→UUID so
// downstream account inserts can stamp dsp_id. profile_type is derived
// from the noise/no_bid knobs (DSPRow.IsCompetitor logic) so we don't
// need a separate `competitor` field on the row.
func (in *inserter) upsertDSPs(ctx context.Context, profiles []DSPProfile) (map[string]string, error) {
	const q = `
INSERT INTO dsps (id, name, display_name, profile_type, noise_pct, no_bid_rate, status, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, 'active', now(), now())
ON CONFLICT (name) DO UPDATE SET
    profile_type = EXCLUDED.profile_type,
    noise_pct    = EXCLUDED.noise_pct,
    no_bid_rate  = EXCLUDED.no_bid_rate,
    updated_at   = now()`
	out := map[string]string{}
	for _, p := range profiles {
		id := DeriveID("dsp", p.Name)
		profileType := "internal"
		if p.NoisePct > 0 || p.NoBidRate > 0 {
			profileType = "competitor"
		}
		if _, err := in.db.ExecContext(ctx, q,
			id, p.Name, displayName(p.Name, "DSP"), profileType,
			int(p.NoisePct), p.NoBidRate,
		); err != nil {
			return nil, fmt.Errorf("upsert dsp %s: %w", p.Name, err)
		}
		out[p.Name] = id
		in.log.Debug("seeded dsp", "name", p.Name, "id", id, "type", profileType)
	}
	return out, nil
}

type ioInsertPayload struct {
	external  string
	accountID string
	name      string
	currency  string
}

func (in *inserter) upsertInsertionOrders(ctx context.Context, ios map[string]ioInsertPayload) error {
	const q = `
INSERT INTO insertion_orders (id, account_id, name, budget, daily_budget, currency, start_date, end_date, status, created_at, updated_at)
VALUES ($1, $2, $3, $4, $4, $5, current_date, current_date + interval '90 days', 'active', now(), now())
ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, updated_at = now()`
	for _, io := range ios {
		if err := in.runTenant(ctx, io.accountID, func(tx *sql.Tx) error {
			id := DeriveID("io", io.external)
			_, err := tx.ExecContext(ctx, q, id, io.accountID, io.name, 100000.0, io.currency)
			return err
		}); err != nil {
			return fmt.Errorf("upsert io %s: %w", io.external, err)
		}
	}
	return nil
}

func (in *inserter) upsertCampaign(ctx context.Context, c CampaignConfig) error {
	accountID := DeriveID("account", c.AccountID)
	ioID := DeriveID("io", c.IOId)
	lineItemID := DeriveID("line_item", c.ID)

	return in.runTenant(ctx, accountID, func(tx *sql.Tx) error {
		// line_items
		const liQ = `
INSERT INTO line_items (
  id, account_id, insertion_order_id, name, status, format, bid_strategy,
  base_bid, bid_currency, daily_budget, pacing_mode, shading_mode,
  creative_rotation, timezone, created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, 'display', $6, $7, $8, $9, $10, 'moderate', 'bandit', 'UTC', now(), now())
ON CONFLICT (id) DO UPDATE SET
  name = EXCLUDED.name, status = EXCLUDED.status, base_bid = EXCLUDED.base_bid,
  daily_budget = EXCLUDED.daily_budget, pacing_mode = EXCLUDED.pacing_mode,
  updated_at = now()`
		_, err := tx.ExecContext(ctx, liQ,
			lineItemID, accountID, ioID, c.Name, defaultStr(c.Status, "live"),
			defaultStr(c.BidModel, "cpm"), c.BaseBid, defaultStr(c.Currency, "USD"),
			c.DailyBudget, defaultStr(c.PacingMode, "even"),
		)
		if err != nil {
			return fmt.Errorf("line_items insert: %w", err)
		}

		// targeting_rules
		const trQ = `
INSERT INTO targeting_rules (
  id, line_item_id, account_id,
  include_geo, exclude_geo, include_device, exclude_device,
  include_segments, exclude_segments,
  include_domains, exclude_domains,
  include_categories, exclude_categories,
  bid_modifiers, created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, now(), now())
ON CONFLICT (line_item_id) DO UPDATE SET
  include_geo = EXCLUDED.include_geo,
  exclude_geo = EXCLUDED.exclude_geo,
  include_device = EXCLUDED.include_device,
  exclude_device = EXCLUDED.exclude_device,
  bid_modifiers = EXCLUDED.bid_modifiers,
  updated_at = now()`
		trID := DeriveID("targeting", c.ID)
		t := c.Targeting
		mods := c.Modifiers
		modsJSON, _ := json.Marshal(modifiersAsMap(mods))
		_, err = tx.ExecContext(ctx, trQ,
			trID, lineItemID, accountID,
			pqStrArr(targetingField(t, "include", "geo")), pqStrArr(targetingField(t, "exclude", "geo")),
			pqStrArr(targetingField(t, "include", "device")), pqStrArr(targetingField(t, "exclude", "device")),
			pqStrArr(targetingField(t, "include", "segments")), pqStrArr(targetingField(t, "exclude", "segments")),
			pqStrArr(targetingField(t, "include", "domains")), pqStrArr(targetingField(t, "exclude", "domains")),
			pqStrArr(targetingField(t, "include", "categories")), pqStrArr(targetingField(t, "exclude", "categories")),
			modsJSON,
		)
		if err != nil {
			return fmt.Errorf("targeting_rules insert: %w", err)
		}

		// creatives — small HTML banners go in html_content directly so the ad
		// server's warm cache has everything it needs without a Minio roundtrip.
		// Larger assets (images, video) point asset_url at Minio instead.
		// Each line item carries one row per size variant: the DSP picks the
		// variant matching the bid request's banner.w/h at bid time. Split
		// between inline HTML and asset_url is 50/50 by creative-id parity
		// (see useAssetURL in creative_assets.go): even suffix → inline,
		// odd suffix → asset_url. Legacy { creative_id, creative_domain }
		// fields are translated into a single-element 300x250 Creatives
		// list so older YAMLs keep working.
		for _, cv := range materialiseCreatives(c) {
			creativeID := DeriveID("creative", cv.ID)
			domain := cv.Domain
			if domain == "" {
				domain = c.CreativeDomain
			}
			landing := landingURLFor(in.landingURLBase, domain)
			format := cv.Format
			if format == "" {
				format = "display"
			}

			// Video/audio: write media URL to asset_url, duration to
			// duration_seconds; html_content is empty so DSP knows
			// it's a non-display creative. Display creatives keep the
			// existing inline-HTML / Minio-asset split.
			var html, assetURL string
			var durationPtr any
			if format == "video" || format == "audio" {
				assetURL = cv.MediaURL
				if cv.Duration > 0 {
					durationPtr = cv.Duration
				}
			} else {
				html = themedCreativeHTML(cv.ID, domain)
				if useAssetForSize(cv.Width, cv.Height) && in.creativeAssetBase != "" {
					key := creativeAssetByTheme(domain, cv.Width, cv.Height)
					assetURL = in.creativeAssetBase + "/" + key
					html = creativeAssetHTML(assetURL)
				}
			}
			const crQ = `
INSERT INTO creatives (
  id, account_id, name, format, width, height, landing_url, html_content, asset_url, duration_seconds, review_status, created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'approved', now(), now())
ON CONFLICT (id) DO UPDATE SET
  name = EXCLUDED.name,
  format = EXCLUDED.format,
  width = EXCLUDED.width,
  height = EXCLUDED.height,
  landing_url = EXCLUDED.landing_url,
  html_content = EXCLUDED.html_content,
  asset_url = EXCLUDED.asset_url,
  duration_seconds = EXCLUDED.duration_seconds,
  updated_at = now()`
			if _, err := tx.ExecContext(ctx, crQ, creativeID, accountID, cv.ID, format, cv.Width, cv.Height, landing, html, assetURL, durationPtr); err != nil {
				return fmt.Errorf("creatives insert (%s): %w", cv.ID, err)
			}

			const linkQ = `
INSERT INTO line_item_creatives (line_item_id, creative_id, weight)
VALUES ($1, $2, 100)
ON CONFLICT (line_item_id, creative_id) DO NOTHING`
			if _, err := tx.ExecContext(ctx, linkQ, lineItemID, creativeID); err != nil {
				return fmt.Errorf("line_item_creatives insert: %w", err)
			}
		}
		return nil
	})
}

// defaultCreativeHTML renders a minimal banner template for a YAML creative.
// Distinct from the ad server's runtime fallback default — this version is
// authored once at seed time and lives in the DB.
func defaultCreativeHTML(externalID, domain string) string {
	if domain == "" {
		domain = "example.com"
	}
	return `<div style="width:${WIDTH}px;height:${HEIGHT}px;background:#f5f5f5;border:1px solid #ddd;display:flex;flex-direction:column;align-items:center;justify-content:center;font-family:sans-serif;">` +
		`<h3 style="margin:0 0 8px;color:#1a1a2e;">` + externalID + `</h3>` +
		`<p style="margin:0 0 12px;color:#666;font-size:13px;">` + domain + `</p>` +
		`<a href="${CLICK_URL}" style="background:#4361ee;color:white;padding:8px 20px;border-radius:4px;text-decoration:none;font-size:13px;">Visit</a>` +
		`<img src="${IMP_PIXEL}" width="1" height="1" style="position:absolute;"></div>`
}

// runTenant opens a tx, sets the RLS context to accountID, runs fn, commits.
func (in *inserter) runTenant(ctx context.Context, accountID string, fn func(*sql.Tx) error) error {
	tx, err := in.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "SELECT set_config('app.current_account_id', $1, true)", accountID); err != nil {
		tx.Rollback()
		return fmt.Errorf("set tenant ctx: %w", err)
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// helpers ---------------------------------------------------------------------

func defaultStr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func displayName(external, kind string) string {
	return kind + " " + external
}

func pqStrArr(s []string) any {
	if len(s) == 0 {
		return pq.StringArray{}
	}
	return pq.StringArray(s)
}

func targetingField(t *TargetingYAML, side, field string) []string {
	if t == nil {
		return nil
	}
	var set TargetingSetYAML
	if side == "include" {
		set = t.Include
	} else {
		set = t.Exclude
	}
	switch field {
	case "geo":
		return set.Geo
	case "device":
		return set.Device
	case "os":
		return set.OS
	case "segments":
		return set.Segments
	case "domains":
		return set.Domains
	case "categories":
		return set.Categories
	}
	return nil
}

func modifiersAsMap(m *ModifiersYAML) map[string]any {
	out := map[string]any{}
	if m == nil {
		return out
	}
	if len(m.Device) > 0 {
		out["device"] = m.Device
	}
	if len(m.GeoCountry) > 0 {
		out["geo_country"] = m.GeoCountry
	}
	return out
}

