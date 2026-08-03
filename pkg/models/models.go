// Package models defines shared domain types used across all services.
//
// These are the core business entities. Services import these types
// for database operations, gRPC conversions, and business logic.
// Proto types (pkg/proto/) are for wire format. Models are for
// internal business logic.
package models

import (
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/targeting"
)

// Campaign is the denormalized read model used by the DSP at bid time.
//
// It joins a line item with its insertion order (for advertiser_id), its
// targeting rules, and its primary creative. The warm cache holds a list
// of these; the bid handler iterates them in process. IDs are UUIDs stored
// as their string text form — keeps Postgres, NATS, logs, and the in-memory
// cache uniform without adding 16-byte parsing on the hot path.
// CampaignCreative is one variant attached to a line item. For display
// it's a size; for video / audio it's a (format + duration + media URL).
// The DSP bidder filters by these fields before submitting a bid —
// campaigns with no matching creative simply no_bid on that request.
// Loaded from line_item_creatives ⋈ creatives at warm-cache time.
type CampaignCreative struct {
	ID       string // creatives.id (UUID)
	Format   string // "display" / "native" / "video" / "audio"
	Width    int
	Height   int
	Duration int    // seconds; video / audio creatives only
	MediaURL string // video / audio media file URL (creatives.asset_url)
	// Native is the asset set for native creatives (creatives.native_assets);
	// nil for non-native formats.
	Native *NativeAssets
}

// NativeAssets is a native creative's content — the values used to fill an
// OpenRTB Native response. Shape mirrors pkg/native.AssetSet; persisted as the
// creatives.native_assets JSONB blob.
type NativeAssets struct {
	Title      string `json:"title"`
	MainImage  string `json:"main_image"`
	MainImageW int    `json:"main_image_w"`
	MainImageH int    `json:"main_image_h"`
	Icon       string `json:"icon"`
	Sponsored  string `json:"sponsored"`
	Body       string `json:"body"`
	CTA        string `json:"cta"`
	LandingURL string `json:"landing_url"`
}

type Campaign struct {
	ID           string // line_items.id (UUID text)
	AccountID    string // owning advertiser account UUID
	AdvertiserID string // = AccountID for advertiser-owned IOs, distinct for agency
	IOId         string // insertion_orders.id
	Name         string
	// CreativeID is the primary (highest-weight) creative's UUID. Kept
	// for backwards compat with callers that don't care about size
	// variants. New code should use Creatives + selectCreativeForSize
	// in the DSP bidder.
	CreativeID string
	// Creatives is the full set of size variants for this line item.
	// At least one entry; ordered by weight DESC.
	Creatives      []CampaignCreative
	CreativeDomain string
	BaseBid        float64
	Currency       string
	DailyBudget    float64
	TotalBudget    float64
	Format         string // display, native, video, audio (line_items.format)
	// ProductCategory is the advertised product's OWN IAB category (line_items.
	// product_category) — the retail relevance signal, distinct from the content-
	// targeting Include.Categories. Empty → the DSP falls back to Include.Categories.
	ProductCategory string
	BidModel       string // cpm, cpc, cpa, vcpm, cpcv
	PacingMode     string // even, asap, front_loaded
	Status         string // live, paused, ended, ...
	// Timezone is the IANA name (line_items.timezone) used to evaluate
	// time-of-day bid modifiers. Empty → UTC.
	Timezone string
	// Location is Timezone pre-resolved to a *time.Location at cache-load time
	// so the bid hot path does no per-request time.LoadLocation. Never nil
	// after a loader resolves it (falls back to time.UTC). json:"-" keeps it
	// out of the campaign API responses.
	Location *time.Location `json:"-"`
	// CreativeRotation is the multi-creative rotation mode (even|weighted|
	// bandit|sequential). Surfaced for the portal's edit prefill.
	CreativeRotation string
	// ViewabilityTargetPct is the contractual viewability guarantee for
	// this line item (0-100). nil = no guarantee. Used downstream for
	// makegood reconciliation; not consulted on the hot bid path.
	ViewabilityTargetPct *int
	Targeting            targeting.Rules
	Modifiers            targeting.Modifiers
}

// ResolveLocation turns an IANA timezone name into a *time.Location, falling
// back to UTC for empty/"UTC"/unknown names. Loaders call it to pre-resolve
// Campaign.Location so the bid hot path never does time.LoadLocation.
func ResolveLocation(tz string) *time.Location {
	if tz == "" || tz == "UTC" {
		return time.UTC
	}
	if loc, err := time.LoadLocation(tz); err == nil {
		return loc
	}
	return time.UTC
}

// FreqCapRule is a campaign's advertiser-configured frequency cap, read from
// the line_item-dimension entry of targeting_rules.frequency_caps. Loaded into
// the ad server's warm cache (keyed by CampaignID) so a serve can enforce the
// advertiser's own limit/window instead of only the platform default.
type FreqCapRule struct {
	CampaignID string
	Limit      int           // max impressions per user per window (0 = no cap)
	Window     time.Duration // rolling window for the counter TTL
}

// Account represents an advertiser, publisher, agency, staff, or admin account.
type Account struct {
	ID        string
	Name      string
	Email     string
	Type      string // advertiser, publisher, agency, staff, admin
	Currency  string // ISO 4217
	Status    string // active, suspended, closed
	CreatedAt time.Time
	UpdatedAt time.Time
}

// InsertionOrder is a budget container above line items.
type InsertionOrder struct {
	ID             string
	AccountID      string
	Name           string
	Budget         float64
	DailyBudget    float64
	Currency       string
	StartDate      time.Time
	EndDate        time.Time
	Status         string // draft, active, paused, ended, archived
	Objective      string // brand_awareness, performance, retargeting
	BudgetRollover bool
	RolloverCapPct int
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// LineItem is a campaign - the entity that has targeting, bids, and creatives.
// "campaign_id" in events and Redis keys refers to LineItem.ID.
type LineItem struct {
	ID               string
	AccountID        string
	InsertionOrderID string
	Name             string
	Status           string // draft, submitted, in_review, rejected, approved, live, paused, ended, archived
	Format           string // display, native, video, audio
	BidStrategy      string // cpm, cpc, cpa, vcpm, cpcv
	BaseBid          float64
	BidCurrency      string
	SubBudget        *float64 // nil = shared IO pool
	DailyBudget      *float64
	PacingMode       string // even, asap, front_loaded
	ShadingMode      string // aggressive, moderate, conservative, disabled
	CreativeRotation string // even, weighted, bandit, sequential
	Timezone         string
	RejectionReason  string
	// ViewabilityTargetPct is the contractual viewability guarantee
	// (0-100). nil = no guarantee.
	ViewabilityTargetPct *int
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// Creative represents an ad creative (image, HTML, video, audio, native).
type Creative struct {
	ID              string
	AccountID       string
	Name            string
	Format          string // display, native, video, audio
	Width           int
	Height          int
	AssetURL        string // Minio/S3 URL
	HTMLContent     string
	LandingURL      string
	DurationSeconds int    // video/audio only
	ReviewStatus    string // uploaded, transcoding, auto_scanning, pending_review, approved, rejected
	RejectionReason string
	ReviewedBy      string
	ReviewedAt      *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// LineItemCreative links a creative to a line item with a rotation weight.
type LineItemCreative struct {
	LineItemID string
	CreativeID string
	Weight     int
}

// Publisher represents a publisher account with revenue share config.
type Publisher struct {
	ID             string
	AccountID      string
	Name           string
	Domain         string
	Currency       string
	Status         string         // pending, active, suspended, closed
	RevShareModel  string         // fixed, tiered, guaranteed_minimum, deal_type, hybrid
	RevShareConfig map[string]any // JSONB
	PaymentTerms   string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Placement represents an ad slot on a publisher's site or app.
type Placement struct {
	ID             string
	PublisherID    string
	AccountID      string
	Name           string
	Format         string // display, native, video, audio, dooh, retail, ingame
	Width          int
	Height         int
	Surfaces       int // in-game: scene surfaces; retail: sponsored slots. 0 = single.
	FloorPrice     float64
	FloorCurrency  string
	PageURLPattern string
	Status         string         // active, inactive
	FloorConfig    map[string]any // JSONB: time-based, device-based, geo-based floors
	VideoConfig    map[string]any // JSONB: skip/duration/mimes/protocols/plcmt for video slots
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Deal represents a PMP, PG, or preferred deal between publisher and advertiser(s).
type Deal struct {
	ID               string
	PublisherID      string
	AccountID        string
	Name             string
	DealType         string // open, pmp, pg, preferred
	Price            float64
	PriceCurrency    string
	AdvertiserIDs    []string
	PlacementIDs     []string
	GuaranteedVolume int64 // for PG deals
	StartDate        *time.Time
	EndDate          *time.Time
	Status           string         // draft, active, paused, ended
	DealConfig       map[string]any // JSONB
	// ViewabilityTargetPct is the publisher-side guarantee on this deal
	// (0-100). nil = no guarantee. PMP/Preferred deals frequently carry
	// this; open auctions don't.
	ViewabilityTargetPct *int
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// AudienceSegment represents a named audience segment.
type AudienceSegment struct {
	ID                 string
	AccountID          string
	Name               string
	Type               string // first_party, behavioral, lookalike, suppression, retargeting, composite, predictive, cdp_imported
	SizeEstimate       int64
	LogicJSON          map[string]any // for composite segments
	Suppression        bool
	TaxonomyCategories []string
	Status             string
	Source             string
	ExpiresAt          *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// BidRequest represents the targeting-relevant signals from an incoming bid request.
// Used by the DSP to evaluate whether to bid.
type BidRequest struct {
	RequestID     string
	PlacementID   string
	PublisherID   string
	PageRequestID string
	Channel       string // display, video, audio, dooh, retail, ingame
	Format        string // banner, native, video, audio, pod, intrinsic
	Width         int
	Height        int
	FloorPrice    float64
	FloorCurrency string

	// Site context
	Domain     string
	PageURL    string
	Categories []string
	Keywords   []string

	// App context
	AppBundle string
	AppName   string

	// Device
	DeviceType     string // mobile, desktop, tablet, ctv
	OS             string
	UserAgent      string
	IP             string
	ConnectionType string

	// Geo
	GeoCountry string
	GeoRegion  string
	GeoCity    string

	// User
	UserID          string
	PublisherUserID string
	HashedEmail     string
	Segments        []string

	// Regulatory
	COPPA         bool
	GDPR          bool
	USPrivacy     string
	ConsentString string
	DataResidency string

	// Video/audio specific
	MinDuration int
	MaxDuration int
	IsLive      bool

	// Deal IDs
	DealIDs []string

	// Inventory type
	InventoryType string // site, app

	TraceID   string
	Timestamp time.Time
}

// BidResponse represents the DSP's response to a bid request.
type BidResponse struct {
	BidID        string
	CampaignID   string // line item ID
	CreativeID   string
	Price        float64
	Currency     string
	BidModel     string // cpm, cpc, cpa, vcpm, cpcv
	AdvertiserID string
	Category     string // IAB category
	DealID       string
	Duration     int // seconds (video/audio)
	LandingURL   string
	NoBid        bool
	NoBidReason  string
}

// ServeRequest is what the exchange/SSP sends to the ad server after an auction win.
type ServeRequest struct {
	TraceID       string  `json:"trace_id"`
	CampaignID    string  `json:"campaign_id"`
	CreativeID    string  `json:"creative_id"`
	PlacementID   string  `json:"placement_id"`
	PublisherID   string  `json:"publisher_id"`
	AdvertiserID  string  `json:"advertiser_id"`
	IOId          string  `json:"io_id"`
	DealID        string  `json:"deal_id"`
	BidModel      string  `json:"bid_model,omitempty"` // cpm, cpc, cpa, vcpm, cpcv — drives tracker billing routing
	ClearingPrice float64 `json:"clearing_price"`
	Currency      string  `json:"currency"`
	SiteDomain    string  `json:"site_domain"`
	Width         int     `json:"width"`
	Height        int     `json:"height"`
	UserID        string  `json:"user_id,omitempty"` // hashed user id; empty = no consent, skip freq cap
	// BehaviourUserID is the user key for consent-gated behavioural capture:
	// set ONLY when the request's regulatory signals permit personalisation
	// (privacy.Evaluate). Distinct from UserID because freq capping runs on
	// contextual-only traffic too (legitimate interest), while a behaviour
	// row is retained profile data. The ad server bakes it into tracker
	// beacons as uid=; the tracker publishes behaviour rows only when present.
	BehaviourUserID string `json:"behaviour_user_id,omitempty"`
	Geo             string `json:"geo,omitempty"`    // request geo (country) — baked into tracker beacons for analytics
	Device          string `json:"device,omitempty"` // request device type — baked into tracker beacons for analytics
	// HouseholdID is the SSP-derived salted-HMAC household id ("hh:…", see
	// pkg/identity.HouseholdID). When present the ad server enforces the
	// frequency cap per household IN ADDITION to per user, so co-viewing
	// devices (CTV + phones on one IP) share one cap. Not PII: keyed hash,
	// same legitimate-interest basis as the user-keyed cap.
	HouseholdID string `json:"household_id,omitempty"`
	// Channel is the ad format ("display"/"video"/"native"/"audio"; empty =
	// display). For non-display channels the ad server has nothing to render
	// (the publisher-adserver builds the VAST/native markup itself), so it runs
	// the frequency cap and returns allowed/declined WITHOUT rendering — making
	// the ad server the single freq-cap authority across every format, including
	// the per-household CTV cap on video.
	Channel string `json:"channel,omitempty"`
	// CapMode controls how the frequency cap is applied on a non-display cap-only
	// call (video/audio/native), where the impression is confirmed LATER (SSAI
	// stitches the ad and fires the impression server-side) rather than at this
	// serve decision. Empty = check-and-record (display, and back-compat). "peek"
	// = decision only, DON'T increment — so a nobid or a cold conditioning-miss
	// never burns a cap slot. "record" = increment only (the ad was actually
	// stitched), no allow/block decision. See CapMode* constants.
	CapMode string `json:"cap_mode,omitempty"`
}

// Frequency-cap application modes for ServeRequest.CapMode (non-display cap-only
// calls). See ServeRequest.CapMode.
const (
	CapModeCheckRecord = ""       // display / default: check + increment at serve
	CapModePeek        = "peek"   // decision only, no increment (video serve decision)
	CapModeRecord      = "record" // increment only, no decision (video stitch = impression)
)

// ServeResponse contains the rendered ad HTML with all macros substituted.
type ServeResponse struct {
	HTML           string  `json:"html"`
	ImpressionURL  string  `json:"impression_url"`
	ClickURL       string  `json:"click_url"`
	ViewabilityURL string  `json:"viewability_url"`
	TraceID        string  `json:"trace_id"`
	CreativeID     string  `json:"creative_id"`
	CampaignID     string  `json:"campaign_id"`
	PlacementID    string  `json:"placement_id"`
	PublisherID    string  `json:"publisher_id"`
	AdvertiserID   string  `json:"advertiser_id"`
	ClearingPrice  float64 `json:"clearing_price"`
	Currency       string  `json:"currency"`
	Width          int     `json:"width"`
	Height         int     `json:"height"`
}

// DSPProfile is a YAML-based DSP configuration with campaign definitions.
type DSPProfile struct {
	Name       string           `yaml:"name" json:"name"`
	Competitor bool             `yaml:"competitor" json:"competitor"`
	NoisePct   float64          `yaml:"noise_pct" json:"noise_pct"`
	NoBidRate  float64          `yaml:"no_bid_rate" json:"no_bid_rate"`
	Campaigns  []CampaignConfig `yaml:"campaigns" json:"campaigns"`
}

// CampaignConfig is a campaign definition in a DSP profile YAML.
type CampaignConfig struct {
	ID             string  `yaml:"id" json:"id"`
	AccountID      string  `yaml:"account_id" json:"account_id"`
	AdvertiserID   string  `yaml:"advertiser_id" json:"advertiser_id"`
	IOId           string  `yaml:"io_id" json:"io_id"`
	Name           string  `yaml:"name" json:"name"`
	CreativeID     string  `yaml:"creative_id" json:"creative_id"`
	CreativeDomain string  `yaml:"creative_domain" json:"creative_domain"`
	BaseBid        float64 `yaml:"base_bid" json:"base_bid"`
	Currency       string  `yaml:"currency" json:"currency"`
	DailyBudget    float64 `yaml:"daily_budget" json:"daily_budget"`
	TotalBudget    float64 `yaml:"total_budget" json:"total_budget"`
	BidModel       string  `yaml:"bid_model" json:"bid_model"`
	PacingMode     string  `yaml:"pacing_mode" json:"pacing_mode"`
	Status         string  `yaml:"status" json:"status"`
}
