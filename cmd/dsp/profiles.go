package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// DSPProfile is the YAML structure for a DSP profile file.
// Different from models.DSPProfile which is the API/storage model.
// This version includes targeting YAML fields for profile loading.
type DSPProfile struct {
	Name      string           `yaml:"name"`
	NoisePct  float64          `yaml:"noise_pct"`
	NoBidRate float64          `yaml:"no_bid_rate"`
	Campaigns []CampaignConfig `yaml:"campaigns"`
	// Competitor field was removed in the v1.2 dsps-table refactor — the
	// behaviour is now derived from noise_pct > 0 || no_bid_rate > 0.
}

// CampaignConfig is a campaign definition in YAML with targeting.
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

// LoadProfile reads a DSP profile from a YAML file.
func LoadProfile(path string) (*DSPProfile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read profile %s: %w", path, err)
	}
	var profile DSPProfile
	if err := yaml.Unmarshal(data, &profile); err != nil {
		return nil, fmt.Errorf("parse profile %s: %w", path, err)
	}
	return &profile, nil
}

// FindProfile looks for a profile YAML in the profiles/dsps/ directory.
func FindProfile(profileName string, log *slog.Logger) (*DSPProfile, error) {
	// Try multiple locations
	paths := []string{
		filepath.Join("profiles", "dsps", profileName+".yaml"),
		filepath.Join("profiles", "dsps", profileName+".yml"),
	}

	for _, p := range paths {
		profile, err := LoadProfile(p)
		if err == nil {
			log.Info("loaded DSP profile from file", "path", p, "campaigns", len(profile.Campaigns))
			return profile, nil
		}
	}

	return nil, fmt.Errorf("profile %q not found in profiles/dsps/", profileName)
}

// (ProfileToCampaigns removed — runtime campaigns now come from the warm cache.
// YAML→models.Campaign conversion lives on yamlCampaignLoader in main.go and is
// only used when Postgres is unreachable.)
