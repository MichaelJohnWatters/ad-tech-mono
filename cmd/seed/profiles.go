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
	CreativeID     string         `yaml:"creative_id"`
	CreativeDomain string         `yaml:"creative_domain"`
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
