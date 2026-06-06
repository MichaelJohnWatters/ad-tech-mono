package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/lib/pq"
	"gopkg.in/yaml.v3"
)

// DirectSoldProfile is the YAML structure for profiles/direct-sold/*.yaml.
type DirectSoldProfile struct {
	Name      string             `yaml:"name"`
	LineItems []DirectSoldLineYAML `yaml:"line_items"`
}

type DirectSoldLineYAML struct {
	ID                   string   `yaml:"id"`
	Publisher            string   `yaml:"publisher"`
	Name                 string   `yaml:"name"`
	DemandSource         string   `yaml:"demand_source"`
	PriorityTier         string   `yaml:"priority_tier"`
	Placements           []string `yaml:"placements"`
	ImpressionsCommitted int64    `yaml:"impressions_committed"`
	DeliveryStart        string   `yaml:"delivery_start"`
	DeliveryEnd          string   `yaml:"delivery_end"`
	CPM                  float64  `yaml:"cpm"`
	PacingMode           string   `yaml:"pacing_mode"`
	LandingURL           string   `yaml:"landing_url"`
	CreativeHTML         string   `yaml:"creative_html"`
}

func LoadDirectSoldProfiles(dir string) ([]DirectSoldProfile, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	var profiles []DirectSoldProfile
	for _, p := range matches {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", p, err)
		}
		var profile DirectSoldProfile
		if err := yaml.Unmarshal(data, &profile); err != nil {
			return nil, fmt.Errorf("parse %s: %w", p, err)
		}
		profiles = append(profiles, profile)
	}
	return profiles, nil
}

// SeedDirectSold UPSERTs every direct-sold line item under its publisher's
// account tenant. Also seeds a creative per line item so the ad server has
// something to render (creatives are owned by the publisher's account, not
// the buyer's).
func (in *inserter) SeedDirectSold(ctx context.Context, profiles []DirectSoldProfile) error {
	for _, p := range profiles {
		for _, li := range p.LineItems {
			if err := in.upsertDirectSold(ctx, li); err != nil {
				return fmt.Errorf("direct-sold %s: %w", li.ID, err)
			}
		}
	}
	return nil
}

func (in *inserter) upsertDirectSold(ctx context.Context, li DirectSoldLineYAML) error {
	pubAccountID := DeriveID("account", li.Publisher)
	publisherID := DeriveID("publisher", li.Publisher)
	lineItemID := DeriveID("publisher_line_item", li.ID)
	creativeID := DeriveID("creative", li.ID+"-creative")

	placementUUIDs := make([]string, 0, len(li.Placements))
	for _, pl := range li.Placements {
		placementUUIDs = append(placementUUIDs, DeriveID("placement", pl))
	}

	var deliveryStart, deliveryEnd sql.NullTime
	if li.DeliveryStart != "" {
		t, err := time.Parse(time.RFC3339, li.DeliveryStart)
		if err != nil {
			return fmt.Errorf("parse delivery_start: %w", err)
		}
		deliveryStart = sql.NullTime{Time: t, Valid: true}
	}
	if li.DeliveryEnd != "" {
		t, err := time.Parse(time.RFC3339, li.DeliveryEnd)
		if err != nil {
			return fmt.Errorf("parse delivery_end: %w", err)
		}
		deliveryEnd = sql.NullTime{Time: t, Valid: true}
	}

	pacing := li.PacingMode
	if pacing == "" {
		pacing = "even"
	}

	return in.runTenant(ctx, pubAccountID, func(tx *sql.Tx) error {
		const crQ = `
INSERT INTO creatives (
  id, account_id, name, format, width, height, landing_url, html_content, review_status, created_at, updated_at
) VALUES ($1, $2, $3, 'display', 0, 0, $4, $5, 'approved', now(), now())
ON CONFLICT (id) DO UPDATE SET
  landing_url = EXCLUDED.landing_url,
  html_content = EXCLUDED.html_content,
  review_status = 'approved',
  updated_at = now()`
		if _, err := tx.ExecContext(ctx, crQ, creativeID, pubAccountID, li.ID, li.LandingURL, li.CreativeHTML); err != nil {
			return fmt.Errorf("creative upsert: %w", err)
		}

		const liQ = `
INSERT INTO publisher_line_items (
  id, account_id, publisher_id, name, demand_source, priority_tier,
  placement_ids, impressions_committed, delivery_start, delivery_end,
  cpm, currency, pacing_mode, status, created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7::uuid[], $8, $9, $10, $11, 'USD', $12, 'active', now(), now())
ON CONFLICT (id) DO UPDATE SET
  name = EXCLUDED.name,
  demand_source = EXCLUDED.demand_source,
  priority_tier = EXCLUDED.priority_tier,
  placement_ids = EXCLUDED.placement_ids,
  impressions_committed = EXCLUDED.impressions_committed,
  delivery_start = EXCLUDED.delivery_start,
  delivery_end = EXCLUDED.delivery_end,
  cpm = EXCLUDED.cpm,
  pacing_mode = EXCLUDED.pacing_mode,
  status = 'active',
  updated_at = now()`
		if _, err := tx.ExecContext(ctx, liQ,
			lineItemID, pubAccountID, publisherID,
			defaultStr(li.Name, li.ID), li.DemandSource, li.PriorityTier,
			pq.StringArray(placementUUIDs), li.ImpressionsCommitted,
			deliveryStart, deliveryEnd,
			li.CPM, pacing,
		); err != nil {
			return fmt.Errorf("publisher_line_items upsert: %w", err)
		}

		const linkQ = `
INSERT INTO publisher_line_item_creatives (publisher_line_item_id, creative_id, weight)
VALUES ($1, $2, 1)
ON CONFLICT (publisher_line_item_id, creative_id) DO NOTHING`
		if _, err := tx.ExecContext(ctx, linkQ, lineItemID, creativeID); err != nil {
			return fmt.Errorf("publisher_line_item_creatives link: %w", err)
		}
		return nil
	})
}
