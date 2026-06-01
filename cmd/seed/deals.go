package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	"github.com/lib/pq"
	"gopkg.in/yaml.v3"
)

// DealsProfile is the YAML structure for profiles/deals/*.yaml.
type DealsProfile struct {
	Name  string     `yaml:"name"`
	Deals []DealYAML `yaml:"deals"`
}

type DealYAML struct {
	ID          string   `yaml:"id"`
	Publisher   string   `yaml:"publisher"`
	Name        string   `yaml:"name"`
	Type        string   `yaml:"type"`
	Price       float64  `yaml:"price"`
	Advertisers []string `yaml:"advertisers"`
	Placements  []string `yaml:"placements"`
}

func LoadDealProfiles(dir string) ([]DealsProfile, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	var profiles []DealsProfile
	for _, p := range matches {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", p, err)
		}
		var profile DealsProfile
		if err := yaml.Unmarshal(data, &profile); err != nil {
			return nil, fmt.Errorf("parse %s: %w", p, err)
		}
		profiles = append(profiles, profile)
	}
	return profiles, nil
}

// SeedDeals UPSERTs every deal under its publisher's account tenant.
//
// Advertiser / placement allowlists are stored as UUID arrays in the deals
// table so the exchange-side matcher can compare directly against the bid's
// advertiser_id (sb.Seat from the OpenRTB response) without a second join.
func (in *inserter) SeedDeals(ctx context.Context, profiles []DealsProfile) error {
	for _, p := range profiles {
		for _, d := range p.Deals {
			if err := in.upsertDeal(ctx, d); err != nil {
				return fmt.Errorf("deal %s: %w", d.ID, err)
			}
		}
	}
	return nil
}

func (in *inserter) upsertDeal(ctx context.Context, d DealYAML) error {
	pubAccountID := DeriveID("account", d.Publisher) // publisher's account
	publisherID := DeriveID("publisher", d.Publisher)
	dealID := DeriveID("deal", d.ID)

	advUUIDs := make([]string, 0, len(d.Advertisers))
	for _, a := range d.Advertisers {
		advUUIDs = append(advUUIDs, DeriveID("account", a))
	}
	placementUUIDs := make([]string, 0, len(d.Placements))
	for _, pl := range d.Placements {
		placementUUIDs = append(placementUUIDs, DeriveID("placement", pl))
	}

	return in.runTenant(ctx, pubAccountID, func(tx *sql.Tx) error {
		const q = `
INSERT INTO deals (
  id, publisher_id, account_id, name, deal_type, price, price_currency,
  advertiser_ids, placement_ids, status, created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, 'USD', $7::uuid[], $8::uuid[], 'active', now(), now())
ON CONFLICT (id) DO UPDATE SET
  name = EXCLUDED.name, deal_type = EXCLUDED.deal_type,
  price = EXCLUDED.price,
  advertiser_ids = EXCLUDED.advertiser_ids,
  placement_ids = EXCLUDED.placement_ids,
  status = EXCLUDED.status,
  updated_at = now()`
		_, err := tx.ExecContext(ctx, q,
			dealID, publisherID, pubAccountID,
			defaultStr(d.Name, d.ID), d.Type, d.Price,
			pq.StringArray(advUUIDs), pq.StringArray(placementUUIDs),
		)
		return err
	})
}
