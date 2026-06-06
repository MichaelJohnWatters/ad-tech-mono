package main

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// DSPProfile mirrors profiles/dsps/*.yaml. Kept local to the seed binary
// since the runtime DSP no longer reads YAML at boot — Postgres + the warm
// cache are the source of truth. This struct is only used by the seeder.
type DSPProfile struct {
	Name      string           `yaml:"name"`
	NoisePct  float64          `yaml:"noise_pct"`
	NoBidRate float64          `yaml:"no_bid_rate"`
	Campaigns []CampaignConfig `yaml:"campaigns"`
	// Competitor was a separate YAML field; removed in the dsps-table refactor
	// (v1.2). competitor: true is now derived from noise_pct > 0 || no_bid_rate > 0.
	// Existing YAMLs may still have the field — yaml.Unmarshal ignores extras
	// so leaving it doesn't break anything.
}

type CampaignConfig struct {
	ID             string         `yaml:"id"`
	AccountID      string         `yaml:"account_id"`
	AdvertiserID   string         `yaml:"advertiser_id"`
	IOId           string         `yaml:"io_id"`
	Name           string         `yaml:"name"`
	// CreativeID + CreativeDomain are the legacy "one 300x250 creative
	// per line item" fields. Still honoured when Creatives is empty so
	// existing YAMLs don't need rewriting. When both legacy + Creatives
	// are set, the Creatives array wins.
	CreativeID     string         `yaml:"creative_id"`
	CreativeDomain string         `yaml:"creative_domain"`
	// Creatives is the new multi-size form. Each entry becomes one row
	// in the creatives table linked to this line item via
	// line_item_creatives. The DSP picks one whose w×h matches the bid
	// request's banner.w/banner.h at bid time, so a single line item
	// can compete on 300x250, 728x90, 970x250 etc. simultaneously.
	Creatives      []CreativeYAML `yaml:"creatives,omitempty"`
	BaseBid        float64        `yaml:"base_bid"`
	Currency       string         `yaml:"currency"`
	DailyBudget    float64        `yaml:"daily_budget"`
	TotalBudget    float64        `yaml:"total_budget"`
	BidModel       string         `yaml:"bid_model"`
	PacingMode     string         `yaml:"pacing_mode"`
	Status         string         `yaml:"status"`
	Targeting      *TargetingYAML `yaml:"targeting,omitempty"`
	Modifiers      *ModifiersYAML `yaml:"modifiers,omitempty"`
}

// CreativeYAML is one size-specific creative under a line item. Domain
// optionally overrides the parent campaign's creative_domain (so a
// single line item could mix brands in theory; in practice every entry
// usually shares the parent's domain). Format defaults to "display".
type CreativeYAML struct {
	ID     string `yaml:"id"`
	Width  int    `yaml:"width"`
	Height int    `yaml:"height"`
	Domain string `yaml:"domain,omitempty"`
	Format string `yaml:"format,omitempty"`
}

type TargetingYAML struct {
	Include TargetingSetYAML `yaml:"include,omitempty"`
	Exclude TargetingSetYAML `yaml:"exclude,omitempty"`
}

type TargetingSetYAML struct {
	Geo        []string `yaml:"geo,omitempty"`
	Device     []string `yaml:"device,omitempty"`
	OS         []string `yaml:"os,omitempty"`
	Segments   []string `yaml:"segments,omitempty"`
	Domains    []string `yaml:"domains,omitempty"`
	Categories []string `yaml:"categories,omitempty"`
}

type ModifiersYAML struct {
	Device     map[string]float64 `yaml:"device,omitempty"`
	GeoCountry map[string]float64 `yaml:"geo_country,omitempty"`
}

// LoadProfiles reads every YAML in dir and returns them sorted by filename.
func LoadProfiles(dir string) ([]DSPProfile, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	var profiles []DSPProfile
	for _, p := range matches {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", p, err)
		}
		var profile DSPProfile
		if err := yaml.Unmarshal(data, &profile); err != nil {
			return nil, fmt.Errorf("parse %s: %w", p, err)
		}
		profiles = append(profiles, profile)
	}
	return profiles, nil
}
