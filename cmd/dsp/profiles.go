package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/targeting"
	"gopkg.in/yaml.v3"
)

// DSPProfile is the YAML structure for a DSP profile file.
type DSPProfile struct {
	Name       string            `yaml:"name"`
	Competitor bool              `yaml:"competitor"`
	NoisePct   float64           `yaml:"noise_pct"`
	NoBidRate  float64           `yaml:"no_bid_rate"`
	Campaigns  []CampaignConfig  `yaml:"campaigns"`
}

// CampaignConfig is a campaign definition in YAML.
type CampaignConfig struct {
	ID             string          `yaml:"id"`
	AccountID      string          `yaml:"account_id"`
	AdvertiserID   string          `yaml:"advertiser_id"`
	IOId           string          `yaml:"io_id"`
	Name           string          `yaml:"name"`
	CreativeID     string          `yaml:"creative_id"`
	CreativeDomain string          `yaml:"creative_domain"`
	BaseBid        float64         `yaml:"base_bid"`
	Currency       string          `yaml:"currency"`
	DailyBudget    float64         `yaml:"daily_budget"`
	TotalBudget    float64         `yaml:"total_budget"`
	BidModel       string          `yaml:"bid_model"`
	PacingMode     string          `yaml:"pacing_mode"`
	Status         string          `yaml:"status"`
	Targeting      *TargetingYAML  `yaml:"targeting,omitempty"`
	Modifiers      *ModifiersYAML  `yaml:"modifiers,omitempty"`
}

type TargetingYAML struct {
	Include TargetingSetYAML `yaml:"include,omitempty"`
	Exclude TargetingSetYAML `yaml:"exclude,omitempty"`
}

type TargetingSetYAML struct {
	Geo       []string `yaml:"geo,omitempty"`
	Device    []string `yaml:"device,omitempty"`
	OS        []string `yaml:"os,omitempty"`
	Segments  []string `yaml:"segments,omitempty"`
	Domains   []string `yaml:"domains,omitempty"`
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

// ProfileToCampaigns converts a YAML profile to the internal Campaign slice.
func ProfileToCampaigns(profile *DSPProfile) []Campaign {
	campaigns := make([]Campaign, 0, len(profile.Campaigns))
	for _, cc := range profile.Campaigns {
		c := Campaign{
			ID:             cc.ID,
			AccountID:      cc.AccountID,
			AdvertiserID:   cc.AdvertiserID,
			IOId:           cc.IOId,
			Name:           cc.Name,
			CreativeID:     cc.CreativeID,
			CreativeDomain: cc.CreativeDomain,
			BaseBid:        cc.BaseBid,
			Currency:       cc.Currency,
			DailyBudget:    cc.DailyBudget,
			TotalBudget:    cc.TotalBudget,
			BidModel:       cc.BidModel,
			PacingMode:     cc.PacingMode,
			Status:         cc.Status,
		}

		if cc.Targeting != nil {
			c.Targeting = targeting.Rules{
				Include: targeting.TargetingSet{
					Geo:        cc.Targeting.Include.Geo,
					Device:     cc.Targeting.Include.Device,
					OS:         cc.Targeting.Include.OS,
					Segments:   cc.Targeting.Include.Segments,
					Domains:    cc.Targeting.Include.Domains,
					Categories: cc.Targeting.Include.Categories,
				},
				Exclude: targeting.TargetingSet{
					Geo:        cc.Targeting.Exclude.Geo,
					Device:     cc.Targeting.Exclude.Device,
					Domains:    cc.Targeting.Exclude.Domains,
					Categories: cc.Targeting.Exclude.Categories,
				},
			}
		}

		if cc.Modifiers != nil {
			c.Modifiers = targeting.Modifiers{
				Device:     cc.Modifiers.Device,
				GeoCountry: cc.Modifiers.GeoCountry,
			}
		}

		campaigns = append(campaigns, c)
	}
	return campaigns
}
