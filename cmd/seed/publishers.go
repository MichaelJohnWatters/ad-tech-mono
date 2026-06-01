package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/lib/pq"
	"gopkg.in/yaml.v3"
)

// PublisherProfile is the YAML structure for profiles/publishers/*.yaml.
type PublisherProfile struct {
	Name       string          `yaml:"name"`
	Publishers []PublisherYAML `yaml:"publishers"`
}

type PublisherYAML struct {
	ID          string          `yaml:"id"`
	Name        string          `yaml:"name"`
	Domain      string          `yaml:"domain"`
	Currency    string          `yaml:"currency"`
	RevSharePct int             `yaml:"revshare_pct"`
	Placements  []PlacementYAML `yaml:"placements"`
}

type PlacementYAML struct {
	ID         string   `yaml:"id"`
	Name       string   `yaml:"name"`
	Format     string   `yaml:"format"`
	Width      int      `yaml:"width"`
	Height     int      `yaml:"height"`
	FloorPrice float64  `yaml:"floor_price"`
	PageURL    string   `yaml:"page_url"`
	Categories []string `yaml:"categories"`
}

// LoadPublisherProfiles reads every YAML in dir.
func LoadPublisherProfiles(dir string) ([]PublisherProfile, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	var profiles []PublisherProfile
	for _, p := range matches {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", p, err)
		}
		var profile PublisherProfile
		if err := yaml.Unmarshal(data, &profile); err != nil {
			return nil, fmt.Errorf("parse %s: %w", p, err)
		}
		profiles = append(profiles, profile)
	}
	return profiles, nil
}

// SeedPublishers UPSERTs accounts → publishers → placements for every YAML
// entry. Each publisher gets its own account row (type='publisher') so RLS
// has a tenant to scope under. Same deterministic-UUID trick as campaigns.
func (in *inserter) SeedPublishers(ctx context.Context, profiles []PublisherProfile) error {
	for _, p := range profiles {
		for _, pub := range p.Publishers {
			if err := in.upsertPublisher(ctx, pub); err != nil {
				return fmt.Errorf("publisher %s: %w", pub.ID, err)
			}
		}
	}
	return nil
}

func (in *inserter) upsertPublisher(ctx context.Context, p PublisherYAML) error {
	accountID := DeriveID("account", p.ID) // publisher account
	publisherID := DeriveID("publisher", p.ID)

	// Account first (no RLS).
	const accQ = `
INSERT INTO accounts (id, name, email, type, currency, status, created_at, updated_at)
VALUES ($1, $2, $3, 'publisher', $4, 'active', now(), now())
ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, updated_at = now()`
	email := p.ID + "@seed.local"
	if _, err := in.db.ExecContext(ctx, accQ, accountID, p.Name, email, defaultStr(p.Currency, "USD")); err != nil {
		return fmt.Errorf("publisher account: %w", err)
	}

	// Publisher + placements under tenant.
	return in.runTenant(ctx, accountID, func(tx *sql.Tx) error {
		revShareJSON, _ := json.Marshal(map[string]any{"fee_pct": defaultInt(p.RevSharePct, 20)})
		const pubQ = `
INSERT INTO publishers (
  id, account_id, name, domain, currency, status, revshare_model, revshare_config, payment_terms, created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, 'active', 'fixed', $6, 'net_30', now(), now())
ON CONFLICT (id) DO UPDATE SET
  name = EXCLUDED.name, domain = EXCLUDED.domain,
  revshare_config = EXCLUDED.revshare_config,
  updated_at = now()`
		if _, err := tx.ExecContext(ctx, pubQ, publisherID, accountID, p.Name, p.Domain, defaultStr(p.Currency, "USD"), revShareJSON); err != nil {
			return fmt.Errorf("publishers insert: %w", err)
		}

		for _, pl := range p.Placements {
			placementID := DeriveID("placement", pl.ID)
			const plQ = `
INSERT INTO placements (
  id, publisher_id, account_id, name, format, width, height, floor_price, floor_currency,
  page_url_pattern, status, floor_config, created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'active', $11, now(), now())
ON CONFLICT (id) DO UPDATE SET
  name = EXCLUDED.name, format = EXCLUDED.format,
  width = EXCLUDED.width, height = EXCLUDED.height,
  floor_price = EXCLUDED.floor_price, floor_currency = EXCLUDED.floor_currency,
  page_url_pattern = EXCLUDED.page_url_pattern,
  floor_config = EXCLUDED.floor_config,
  updated_at = now()`
			catsJSON, _ := json.Marshal(map[string]any{"categories": pl.Categories})
			if _, err := tx.ExecContext(ctx, plQ,
				placementID, publisherID, accountID,
				defaultStr(pl.Name, pl.ID), defaultStr(pl.Format, "display"),
				pl.Width, pl.Height, pl.FloorPrice, defaultStr(p.Currency, "USD"),
				pl.PageURL, catsJSON,
			); err != nil {
				return fmt.Errorf("placements insert: %w", err)
			}
		}
		return nil
	})
}

func defaultInt(v, fallback int) int {
	if v == 0 {
		return fallback
	}
	return v
}

// Suppress unused import warnings if pq is removed later.
var _ = pq.StringArray{}
