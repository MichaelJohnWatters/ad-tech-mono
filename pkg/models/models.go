// Package models defines shared domain types used across all services.
//
// These are the core business entities. Services import these types
// for database operations, gRPC conversions, and business logic.
// Proto types (pkg/proto/) are for wire format. Models are for
// internal business logic.
package models

import (
	"time"
)

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
	ID               string
	AccountID        string
	Name             string
	Budget           float64
	DailyBudget      float64
	Currency         string
	StartDate        time.Time
	EndDate          time.Time
	Status           string // draft, active, paused, ended, archived
	Objective        string // brand_awareness, performance, retargeting
	BudgetRollover   bool
	RolloverCapPct   int
	CreatedAt        time.Time
	UpdatedAt        time.Time
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
	CreatedAt        time.Time
	UpdatedAt        time.Time
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
	DurationSeconds int // video/audio only
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
	ID              string
	AccountID       string
	Name            string
	Domain          string
	Currency        string
	Status          string // pending, active, suspended, closed
	RevShareModel   string // fixed, tiered, guaranteed_minimum, deal_type, hybrid
	RevShareConfig  map[string]any // JSONB
	PaymentTerms    string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Placement represents an ad slot on a publisher's site or app.
type Placement struct {
	ID            string
	PublisherID   string
	AccountID     string
	Name          string
	Format        string // display, native, video, audio, dooh
	Width         int
	Height        int
	FloorPrice    float64
	FloorCurrency string
	PageURLPattern string
	Status        string // active, inactive
	FloorConfig   map[string]any // JSONB: time-based, device-based, geo-based floors
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Deal represents a PMP, PG, or preferred deal between publisher and advertiser(s).
type Deal struct {
	ID              string
	PublisherID     string
	AccountID       string
	Name            string
	DealType        string // open, pmp, pg, preferred
	Price           float64
	PriceCurrency   string
	AdvertiserIDs   []string
	PlacementIDs    []string
	GuaranteedVolume int64 // for PG deals
	StartDate       *time.Time
	EndDate         *time.Time
	Status          string // draft, active, paused, ended
	DealConfig      map[string]any // JSONB
	CreatedAt       time.Time
	UpdatedAt       time.Time
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
	AppBundle  string
	AppName    string

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
	COPPA          bool
	GDPR           bool
	USPrivacy      string
	ConsentString  string
	DataResidency  string

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
	BidID         string
	CampaignID    string // line item ID
	CreativeID    string
	Price         float64
	Currency      string
	BidModel      string // cpm, cpc, cpa, vcpm, cpcv
	AdvertiserID  string
	Category      string // IAB category
	DealID        string
	Duration      int // seconds (video/audio)
	LandingURL    string
	NoBid         bool
	NoBidReason   string
}
