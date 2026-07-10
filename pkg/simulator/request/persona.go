// Package request is the single source of truth for building "production-like"
// OpenRTB bid requests for the traffic simulator. A thin request (a bare banner
// imp + geo) exercises almost none of the platform's decision paths; this
// package populates every field a real service actually reads — supply chain,
// consent/regulatory signals, identity (UID2 / hashed email / first-party id),
// audience segments, and a format-appropriate impression — so a simulated
// request drives the same code as production traffic.
//
// The unit of realism is the Persona: a coherent bundle of who the user is
// (geo, device, identity type, audience segments) and what privacy regime
// applies to them. Combined with a Placement (where the ad runs) and a Channel
// (display/video/audio/native), Build produces a fully-populated
// openrtb.BidRequest.
//
// Both the CLI simulator (which POSTs OpenRTB straight to the exchange) and any
// other Go caller use Build directly. The browser-driven web-UI paths can't
// build OpenRTB themselves — the SSP/publisher-adserver do that server-side —
// so Persona also exposes QueryParams (see querybuild.go) to carry the same
// signals through as request parameters.
package request

import "math/rand"

// Channel is the ad format/channel a simulated request targets. It selects
// which impression object Build attaches (banner/video/audio/native) and the
// ImpExt.channel tag.
type Channel string

const (
	Display Channel = "display"
	Video   Channel = "video"
	Audio   Channel = "audio"
	Native  Channel = "native"
)

// AllChannels lists every channel Build can produce, for CLI validation and
// profile mixes.
var AllChannels = []Channel{Display, Video, Audio, Native}

// Regime is the privacy/regulatory regime a persona carries. Each maps to a
// concrete set of Regs + User.Ext.Consent signals that the platform's privacy
// engine (pkg/privacy) evaluates to a bid/contextual-only/no-bid decision, so a
// persona mix exercises every branch of privacy.Evaluate.
type Regime string

const (
	// RegimeUSClear is US traffic with no privacy law in play → full
	// personalisation. The common baseline.
	RegimeUSClear Regime = "us_clear"
	// RegimeGDPRConsented is EU traffic (gdpr=1) with a TCF consent string
	// present → the DSP may personalise.
	RegimeGDPRConsented Regime = "gdpr_consented"
	// RegimeGDPRNoConsent is EU traffic (gdpr=1) with no consent string →
	// privacy engine downgrades to contextual-only.
	RegimeGDPRNoConsent Regime = "gdpr_no_consent"
	// RegimeCCPAOptOut carries a US Privacy string with the sale opt-out bit
	// set ("1YYN") → contextual-only.
	RegimeCCPAOptOut Regime = "ccpa_opt_out"
	// RegimeGPC asserts the Global Privacy Control browser signal → contextual-only.
	RegimeGPC Regime = "gpc"
	// RegimeCOPPA is child-directed (regs.coppa=1) → contextual-only.
	RegimeCOPPA Regime = "coppa"
	// RegimeGPPOptOut carries a GPP US-National string encoding a sale opt-out
	// → contextual-only.
	RegimeGPPOptOut Regime = "gpp_opt_out"
)

// Identity is the kind of stable identifier a persona presents. It decides
// whether the request is addressable (User.ID / UID2 / hashed email) or fully
// anonymous, which drives the DSP's user key, opt-out lookup, frequency capping,
// and — on the SSP path — identity-graph edge creation.
type Identity string

const (
	// IdentityAnonymous carries no user identifier at all (cookieless, no
	// consented id). Only contextual signals are available.
	IdentityAnonymous Identity = "anonymous"
	// IdentityPublisherID carries a first-party User.ID (publisher's own id).
	IdentityPublisherID Identity = "publisher_id"
	// IdentityUID2 carries a Unified ID 2.0 token in User.EIDs (no first-party id).
	IdentityUID2 Identity = "uid2"
	// IdentityHashedEmail carries a SHA-256 hashed email in User.Ext (a
	// deterministic identity-graph signal).
	IdentityHashedEmail Identity = "hashed_email"
)

// Persona is a coherent, reusable description of one kind of user. Personas are
// the building block of a realistic traffic mix: each run samples personas by
// weight so the resulting requests span geos, devices, identity types, and
// privacy regimes the way real traffic does.
type Persona struct {
	// Name is a stable slug ("eu-consented-mobile") used on the CLI and in logs.
	Name string
	// Regime + Identity select the privacy signals and identifier type.
	Regime   Regime
	Identity Identity
	// Geo is the ISO 3166-1 alpha-3 country; Region/City are optional finer geo.
	Geo    string
	Region string
	City   string
	// Device is mobile/desktop/tablet/ctv; OS/Make/Model flesh out the device.
	Device string
	OS     string
	Make   string
	Model  string
	// Segments are audience-segment ids the user belongs to (behavioural
	// targeting, only honoured when the privacy regime permits personalisation).
	Segments []string
	// Weight is the relative sampling frequency in a mix (higher = more common).
	Weight int
}

// Personas is the default registry: a realistic spread across regions, devices,
// identity types, and privacy regimes. Weights approximate real open-exchange
// traffic — the US-clear personalisable cases dominate, with a long tail of
// consent-constrained and anonymous traffic that exercises the privacy and
// contextual-fallback paths.
var Personas = []Persona{
	{
		Name: "us-personalised-mobile", Regime: RegimeUSClear, Identity: IdentityUID2,
		Geo: "USA", Region: "NY", City: "New York", Device: "mobile", OS: "iOS", Make: "Apple", Model: "iPhone15,3",
		Segments: []string{"in_market_auto", "sports_enthusiast"}, Weight: 24,
	},
	{
		Name: "us-personalised-desktop", Regime: RegimeUSClear, Identity: IdentityHashedEmail,
		Geo: "USA", Region: "CA", City: "San Francisco", Device: "desktop", OS: "macOS", Make: "Apple",
		Segments: []string{"finance_intender", "high_income"}, Weight: 18,
	},
	{
		Name: "us-firstparty-tablet", Regime: RegimeUSClear, Identity: IdentityPublisherID,
		Geo: "USA", Region: "TX", City: "Austin", Device: "tablet", OS: "Android", Make: "Samsung", Model: "SM-T870",
		Segments: []string{"parenting", "in_market_auto"}, Weight: 8,
	},
	{
		Name: "us-ccpa-optout", Regime: RegimeCCPAOptOut, Identity: IdentityPublisherID,
		Geo: "USA", Region: "CA", City: "Los Angeles", Device: "mobile", OS: "Android", Make: "Google", Model: "Pixel 8",
		Segments: []string{"sports_enthusiast"}, Weight: 6,
	},
	{
		Name: "us-gpc-optout", Regime: RegimeGPC, Identity: IdentityHashedEmail,
		Geo: "USA", Region: "WA", City: "Seattle", Device: "desktop", OS: "Windows",
		Segments: []string{"tech_early_adopter"}, Weight: 4,
	},
	{
		Name: "us-gpp-optout", Regime: RegimeGPPOptOut, Identity: IdentityPublisherID,
		Geo: "USA", Region: "IL", City: "Chicago", Device: "mobile", OS: "iOS", Make: "Apple", Model: "iPhone14,5",
		Segments: []string{"travel_intender"}, Weight: 3,
	},
	{
		Name: "eu-consented-mobile", Regime: RegimeGDPRConsented, Identity: IdentityUID2,
		Geo: "DEU", Region: "BE", City: "Berlin", Device: "mobile", OS: "Android", Make: "Samsung", Model: "SM-S911B",
		Segments: []string{"in_market_auto", "luxury_goods"}, Weight: 10,
	},
	{
		Name: "eu-noconsent-desktop", Regime: RegimeGDPRNoConsent, Identity: IdentityAnonymous,
		Geo: "FRA", Region: "IDF", City: "Paris", Device: "desktop", OS: "Windows", Weight: 8,
	},
	{
		Name: "uk-consented-desktop", Regime: RegimeGDPRConsented, Identity: IdentityHashedEmail,
		Geo: "GBR", Region: "ENG", City: "London", Device: "desktop", OS: "macOS", Make: "Apple",
		Segments: []string{"finance_intender", "travel_intender"}, Weight: 9,
	},
	{
		Name: "coppa-kids-tablet", Regime: RegimeCOPPA, Identity: IdentityAnonymous,
		Geo: "USA", Region: "FL", City: "Miami", Device: "tablet", OS: "iPadOS", Make: "Apple", Model: "iPad13,1", Weight: 2,
	},
	{
		Name: "us-ctv-household", Regime: RegimeUSClear, Identity: IdentityPublisherID,
		Geo: "USA", Region: "GA", City: "Atlanta", Device: "ctv", OS: "Roku", Make: "Roku", Model: "Ultra",
		Segments: []string{"sports_enthusiast", "streaming_subscriber"}, Weight: 5,
	},
	{
		Name: "anon-mobile-open", Regime: RegimeUSClear, Identity: IdentityAnonymous,
		Geo: "USA", Region: "OH", City: "Columbus", Device: "mobile", OS: "Android", Make: "Motorola", Weight: 6,
	},
}

// PersonaByName returns the registry persona with the given name and whether it
// was found.
func PersonaByName(name string) (Persona, bool) {
	for _, p := range Personas {
		if p.Name == name {
			return p, true
		}
	}
	return Persona{}, false
}

// PersonaNames returns every registered persona name, for CLI listing/validation.
func PersonaNames() []string {
	names := make([]string, len(Personas))
	for i, p := range Personas {
		names[i] = p.Name
	}
	return names
}

// Pick samples a persona from personas by weight using rng. Personas with a
// non-positive weight are treated as weight 1 so they can still appear. Returns
// the zero Persona only when personas is empty.
func Pick(rng *rand.Rand, personas []Persona) Persona {
	total := 0
	for _, p := range personas {
		total += weight(p)
	}
	if total == 0 {
		return Persona{}
	}
	n := rng.Intn(total)
	for _, p := range personas {
		n -= weight(p)
		if n < 0 {
			return p
		}
	}
	return personas[len(personas)-1]
}

func weight(p Persona) int {
	if p.Weight <= 0 {
		return 1
	}
	return p.Weight
}
