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
	ID           string `yaml:"id"`
	AccountID    string `yaml:"account_id"`
	AdvertiserID string `yaml:"advertiser_id"`
	IOId         string `yaml:"io_id"`
	Name         string `yaml:"name"`
	// CreativeID + CreativeDomain are the legacy "one 300x250 creative
	// per line item" fields. Still honoured when Creatives is empty so
	// existing YAMLs don't need rewriting. When both legacy + Creatives
	// are set, the Creatives array wins.
	CreativeID     string `yaml:"creative_id"`
	CreativeDomain string `yaml:"creative_domain"`
	// Creatives is the new multi-size form. Each entry becomes one row
	// in the creatives table linked to this line item via
	// line_item_creatives. The DSP picks one whose w×h matches the bid
	// request's banner.w/banner.h at bid time, so a single line item
	// can compete on 300x250, 728x90, 970x250 etc. simultaneously.
	Creatives   []CreativeYAML `yaml:"creatives,omitempty"`
	BaseBid     float64        `yaml:"base_bid"`
	Currency    string         `yaml:"currency"`
	DailyBudget float64        `yaml:"daily_budget"`
	TotalBudget float64        `yaml:"total_budget"`
	BidModel    string         `yaml:"bid_model"`
	PacingMode  string         `yaml:"pacing_mode"`
	// ShadingMode opts a line item into bid shading (disabled / conservative /
	// moderate / aggressive). Defaults to "disabled" (opt-in). When set, the DSP
	// lowers the bid toward the clearing price to avoid overpaying — see
	// pkg/bidshading. The shading-showcase seed campaigns set this to demonstrate
	// real, durable "dollars saved" on the advertiser portal.
	ShadingMode string `yaml:"shading_mode,omitempty"`
	Status      string `yaml:"status"`
	// Format is the line item's primary format (line_items.format). Defaults
	// to "display"; set to "native"/"video"/"audio" for those line items.
	Format    string         `yaml:"format,omitempty"`
	Targeting *TargetingYAML `yaml:"targeting,omitempty"`
	Modifiers *ModifiersYAML `yaml:"modifiers,omitempty"`
}

// CreativeYAML is one creative variant under a line item.
//
// For display creatives: Width, Height define the slot size; the seed
// inserter writes inline themed HTML or wraps a Minio-hosted SVG based
// on the size split. Format defaults to "display".
//
// For video / audio creatives: set Format to "video" or "audio",
// MediaURL to the MP4/WebM/MP3 URL the player should fetch, and
// Duration to the playback length in seconds. Width/Height stay set on
// video so a 640x360 creative matches a 640x360 video slot — the
// player respects those even for letterboxing.
//
// Domain optionally overrides the parent campaign's creative_domain
// (every entry in practice shares the parent's domain).
type CreativeYAML struct {
	ID       string `yaml:"id"`
	Name     string `yaml:"name,omitempty"` // display name in the creative library; falls back to ID
	Width    int    `yaml:"width"`
	Height   int    `yaml:"height"`
	Domain   string `yaml:"domain,omitempty"`
	Format   string `yaml:"format,omitempty"`    // "display" (default), "native", "video", "audio"
	MediaURL string `yaml:"media_url,omitempty"` // video/audio creatives only — URL the player fetches
	Duration int    `yaml:"duration,omitempty"`  // seconds; video/audio only
	// Native holds the asset set for native creatives (format: native),
	// persisted to creatives.native_assets.
	Native *NativeYAML `yaml:"native,omitempty"`
}

// NativeYAML is the native creative's asset content. Mirrors
// models.NativeAssets / pkg/native.AssetSet.
type NativeYAML struct {
	Title      string `yaml:"title,omitempty"`
	MainImage  string `yaml:"main_image,omitempty"`
	MainImageW int    `yaml:"main_image_w,omitempty"`
	MainImageH int    `yaml:"main_image_h,omitempty"`
	Icon       string `yaml:"icon,omitempty"`
	Sponsored  string `yaml:"sponsored,omitempty"`
	Body       string `yaml:"body,omitempty"`
	CTA        string `yaml:"cta,omitempty"`
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
	// Audience maps segment EXTERNAL keys ("seg-dog-lovers", "synthetic-042")
	// to bid-modifier percentages. modifiersAsMap derives the segment UUIDs —
	// the DSP's ModifierContext carries segment UUIDs, so raw keys would
	// never match (same rule as targeting include_segments).
	Audience map[string]float64 `yaml:"audience,omitempty"`
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
