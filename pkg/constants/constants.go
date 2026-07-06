// Package constants defines shared keywords, status values, and naming
// conventions used across all services. Import this instead of hardcoding
// strings like "live", "cpm", "mobile" in each service.
package constants

// ============================================================
// Account Types
// ============================================================

const (
	AccountAdvertiser = "advertiser"
	AccountPublisher  = "publisher"
	AccountAgency     = "agency"
	AccountStaff      = "staff"
	AccountAdmin      = "admin"
)

// ============================================================
// Campaign / Line Item Status
// ============================================================

const (
	StatusDraft     = "draft"
	StatusSubmitted = "submitted"
	StatusInReview  = "in_review"
	StatusRejected  = "rejected"
	StatusApproved  = "approved"
	StatusLive      = "live"
	StatusPaused    = "paused"
	StatusEnded     = "ended"
	StatusArchived  = "archived"
)

// ============================================================
// Bid Models
// ============================================================

const (
	BidModelCPM  = "cpm"
	BidModelCPC  = "cpc"
	BidModelCPA  = "cpa"
	BidModelVCPM = "vcpm"
	BidModelCPCV = "cpcv"
	BidModelCPI  = "cpi"
)

// ============================================================
// Pacing Modes
// ============================================================

const (
	PacingEven        = "even"
	PacingASAP        = "asap"
	PacingFrontLoaded = "front_loaded"
)

// ============================================================
// Device Types
// ============================================================

const (
	DeviceMobile  = "mobile"
	DeviceDesktop = "desktop"
	DeviceTablet  = "tablet"
	DeviceCTV     = "ctv"
)

// OpenRTB device type integers
const (
	DeviceTypeOrtbMobile  = 1
	DeviceTypeOrtbDesktop = 2
	DeviceTypeOrtbCTV     = 3
	DeviceTypeOrtbTablet  = 5
)

// DeviceTypeFromOrtb converts OpenRTB device type int to string.
func DeviceTypeFromOrtb(dt int) string {
	switch dt {
	case DeviceTypeOrtbMobile:
		return DeviceMobile
	case DeviceTypeOrtbDesktop:
		return DeviceDesktop
	case DeviceTypeOrtbCTV:
		return DeviceCTV
	case DeviceTypeOrtbTablet:
		return DeviceTablet
	default:
		return DeviceDesktop
	}
}

// DeviceTypeToOrtb converts string to OpenRTB device type int.
func DeviceTypeToOrtb(device string) int {
	switch device {
	case DeviceMobile:
		return DeviceTypeOrtbMobile
	case DeviceDesktop:
		return DeviceTypeOrtbDesktop
	case DeviceCTV:
		return DeviceTypeOrtbCTV
	case DeviceTablet:
		return DeviceTypeOrtbTablet
	default:
		return DeviceTypeOrtbDesktop
	}
}

// ============================================================
// Channels
// ============================================================

const (
	ChannelDisplay = "display"
	ChannelVideo   = "video"
	ChannelAudio   = "audio"
	ChannelNative  = "native"
	ChannelDOOH    = "dooh"
	ChannelCTV     = "ctv"
	ChannelRetail  = "retail"
	ChannelInGame  = "ingame"
	ChannelAll     = "all"
)

// ============================================================
// Creative Formats
// ============================================================

const (
	FormatBanner       = "banner"
	FormatNative       = "native"
	FormatVideo        = "video"
	FormatAudio        = "audio"
	FormatInterstitial = "interstitial"
	FormatRewarded     = "rewarded"
)

// ============================================================
// Inventory Type
// ============================================================

const (
	InventorySite = "site"
	InventoryApp  = "app"
)

// ============================================================
// Deal Types
// ============================================================

const (
	DealOpen      = "open"
	DealPMP       = "pmp"
	DealPG        = "pg"
	DealPreferred = "preferred"
)

// ============================================================
// Currencies
// ============================================================

const (
	CurrencyUSD = "USD"
	CurrencyGBP = "GBP"
	CurrencyEUR = "EUR"
	CurrencyJPY = "JPY"
)

// ============================================================
// Event Types (tracker / analytics)
// ============================================================

const (
	EventImpression = "impression"
	EventClick      = "click"
	EventConversion = "conversion"
	EventViewable   = "viewable"
	EventVideoStart = "start"
	EventVideoQ1    = "firstQuartile"
	EventVideoMid   = "midpoint"
	EventVideoQ3    = "thirdQuartile"
	EventVideoEnd   = "complete"
	EventVideoSkip  = "skip"
	EventAudioStart = "audio_start"
	EventAudioEnd   = "audio_complete"
)

// ============================================================
// Conversion Types
// ============================================================

const (
	ConvPurchase = "purchase"
	ConvSignup   = "signup"
	ConvInstall  = "install"
	ConvCustom   = "custom"
)

// ============================================================
// Privacy / Consent
// ============================================================

const (
	ConsentGranted = "granted"
	ConsentDenied  = "denied"
	ConsentUnknown = "unknown"

	OptOutLevel1 = 1 // no personalisation
	OptOutLevel2 = 2 // no tracking
	OptOutLevel3 = 3 // full deletion
)

// ============================================================
// Fraud Categories (IAB IVT)
// ============================================================

const (
	FraudClean = "clean"
	FraudGIVT  = "givt" // General Invalid Traffic
	FraudSIVT  = "sivt" // Sophisticated Invalid Traffic
)

// ============================================================
// Revenue Share Models
// ============================================================

const (
	RevenueFixed      = "fixed"
	RevenueTiered     = "tiered"
	RevenueGuaranteed = "guaranteed"
	RevenueDealType   = "deal_type"
	RevenueHybrid     = "hybrid"
)

// ============================================================
// Ledger Entry Types
// ============================================================

const (
	LedgerSpend       = "spend"
	LedgerReservation = "reservation"
	LedgerSettlement  = "settlement"
	LedgerRelease     = "release"
	LedgerAdjustment  = "adjustment"
	LedgerRefund      = "refund"
)

// ============================================================
// Audience Segment Types
// ============================================================

const (
	SegmentFirstParty  = "first_party"
	SegmentBehavioural = "behavioural"
	SegmentLookalike   = "lookalike"
	SegmentComposite   = "composite"
	SegmentSuppression = "suppression"
)

// HTTP constants are in http.go (same package)

// ============================================================
// Config Keys (standard keys used across services)
// ============================================================

const (
	CfgPort               = "port"
	CfgNATSURL            = "nats_url"
	CfgReportingURL       = "reporting_url"
	CfgExchangeURL        = "exchange_url"
	CfgTrackerURL         = "tracker_url"
	CfgAdServerURL        = "adserver_url"
	CfgSigningKey         = "signing_key"
	CfgJWTSigningKey      = "jwt_signing_key"
	CfgConfigPollInterval = "config_poll_interval"
	CfgBidTimeout         = "bid_timeout"
	CfgDSPEndpoints       = "dsp_endpoints"
	CfgDSPProfile         = "profile"
)

// ============================================================
// Service Names
// ============================================================

const (
	ServiceGateway           = "gateway"
	ServiceExchange          = "exchange"
	ServiceDSP               = "dsp"
	ServiceTracker           = "tracker"
	ServiceSSP               = "ssp"
	ServiceAdServer          = "adserver"
	ServiceReporting         = "reporting"
	ServicePipeline          = "pipeline"
	ServiceSeed              = "seed"
	ServiceBilling           = "billing"
	ServiceWebhooks          = "webhooks"
	ServiceFraud             = "fraud"
	ServicePrivacy           = "privacy"
	ServicePublisherAdServer = "publisher-adserver"
	ServiceIdentityConsumer  = "identity-consumer"
)

// ============================================================
// Geo (common test geos)
// ============================================================

const (
	GeoGBR = "GBR"
	GeoUSA = "USA"
	GeoDEU = "DEU"
	GeoFRA = "FRA"
	GeoJPN = "JPN"
)

// ============================================================
// IAB Content Categories (common ones)
// ============================================================

// ============================================================
// NATS consumer group names
// ============================================================
//
// Passed as the `group` argument to bus.Subscribe. The natsbus wrapper
// composes the actual JetStream consumer name as {service}-{group}-{subject-leaf}
// so different subjects under the same group don't collide.

const (
	// NATSGroupReporting — reporting service's event-ingestion consumers
	// (impression, click, conversion, auction.complete, auction.win).
	NATSGroupReporting = "reporting"
	// NATSGroupWebhooks — webhooks dispatcher's consumers of account-scoped
	// business events (budget/balance depleted, campaign state changed).
	NATSGroupWebhooks = "webhooks"
	// NATSGroupIdentityConsumer — identity-consumer's consumer of observed
	// identity signals. Keep a single replica so the probabilistic fingerprint
	// buckets stay a coherent global view (queue-group load-balancing would
	// split them across replicas).
	NATSGroupIdentityConsumer = "identity-consumer"
)

// ============================================================
// IAB Content Categories — complete tier-1 set (IAB Content Taxonomy 1.0).
// Codes/names per the IAB spec; pkg/taxonomy is the authoritative lookup +
// validation API. Named aliases here are for readable references in code.
// Tier-2 examples (e.g. IABSportsSoccer) are illustrative, not exhaustive.
// ============================================================

const (
	IABEntertainment = "IAB1"  // Arts & Entertainment
	IABAuto          = "IAB2"  // Automotive
	IABBusiness      = "IAB3"  // Business
	IABCareers       = "IAB4"  // Careers
	IABEducation     = "IAB5"  // Education
	IABFamily        = "IAB6"  // Family & Parenting
	IABHealth        = "IAB7"  // Health & Fitness
	IABFood          = "IAB8"  // Food & Drink
	IABHobbies       = "IAB9"  // Hobbies & Interests
	IABHomeGarden    = "IAB10" // Home & Garden
	IABLawPolitics   = "IAB11" // Law, Government & Politics
	IABNews          = "IAB12" // News
	IABFinance       = "IAB13" // Personal Finance
	IABSociety       = "IAB14" // Society
	IABScience       = "IAB15" // Science
	IABPets          = "IAB16" // Pets
	IABSports        = "IAB17" // Sports
	IABFashion       = "IAB18" // Style & Fashion
	IABTech          = "IAB19" // Technology & Computing
	IABTravel        = "IAB20" // Travel
	IABRealEstate    = "IAB21" // Real Estate (NOT IAB10 — that's Home & Garden)
	IABShopping      = "IAB22" // Shopping
	IABReligion      = "IAB23" // Religion & Spirituality
	IABUncategorized = "IAB24" // Uncategorized
	IABNonStandard   = "IAB25" // Non-Standard Content
	IABIllegal       = "IAB26" // Illegal Content

	// Gaming has no dedicated tier-1 in Taxonomy 1.0; it lives under
	// Hobbies & Interests (IAB9-30 Video & Computer Games).
	IABGaming = "IAB9"

	// Illustrative tier-2 subcategories used by the contextual classifier.
	IABSportsSoccer = "IAB17-1"
)
