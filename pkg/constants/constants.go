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
	CfgPort             = "port"
	CfgNATSURL          = "nats_url"
	CfgReportingURL     = "reporting_url"
	CfgExchangeURL      = "exchange_url"
	CfgTrackerURL       = "tracker_url"
	CfgAdServerURL      = "adserver_url"
	CfgSigningKey       = "signing_key"
	CfgJWTSigningKey    = "jwt_signing_key"
	CfgConfigPollInterval = "config_poll_interval"
	CfgBidTimeout       = "bid_timeout"
	CfgDSPEndpoints     = "dsp_endpoints"
	CfgDSPProfile       = "profile"
)

// ============================================================
// Service Names
// ============================================================

const (
	ServiceGateway          = "gateway"
	ServiceExchange         = "exchange"
	ServiceDSP              = "dsp"
	ServiceTracker          = "tracker"
	ServiceSSP              = "ssp"
	ServiceAdServer         = "adserver"
	ServiceReporting        = "reporting"
	ServicePipeline         = "pipeline"
	ServiceSeed             = "seed"
	ServiceBilling          = "billing"
	ServiceWebhooks         = "webhooks"
	ServiceFraud            = "fraud"
	ServicePrivacy          = "privacy"
	ServicePublisherAdServer = "publisher-adserver"
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
)

// ============================================================
// IAB Content Categories (common ones)
// ============================================================

const (
	IABNews        = "IAB12"
	IABSports      = "IAB17"
	IABSportsSoccer = "IAB17-1"
	IABTech        = "IAB19"
	IABFinance     = "IAB13"
	IABAuto        = "IAB2"
	IABTravel      = "IAB20"
	IABFood        = "IAB8"
	IABHealth      = "IAB7"
	IABFashion     = "IAB18"
	IABEntertainment = "IAB1"
	IABEducation   = "IAB5"
	IABProperty    = "IAB10"
	IABGaming      = "IAB9"
	IABShopping    = "IAB22"
)
