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

import (
	"fmt"
	"math/rand"
	"time"
)

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
	// IP is a stable TEST-NET client address for the persona, sent as ?ip= so
	// the SSP's household derivation is DETERMINISTIC per persona: the same
	// persona always lands in the same household, and two personas can share
	// an IP to model one household with multiple viewers. Empty = no ip param.
	IP string
	// HouseholdPool > 1 makes the persona represent a POPULATION: each
	// request draws its IP from a pool of this many addresses derived from
	// IP's prefix, so household frequency caps see many households sharing
	// one profile. 0/1 keeps the single deterministic household (the CTV
	// co-viewing personas depend on that). Without a pool, 13 personas = 13
	// households: at load-run rates the serve layer freq-capped ~half of
	// all EXCHANGE-CLEARED wins (2026-07-19 run #2 — 100k cleared, 52k
	// served), which no real population would.
	HouseholdPool int
	// HouseholdShared picks the pool slot from the WALL CLOCK (10-minute
	// slots) instead of randomly, so two personas with the same IP + pool
	// land in the SAME household at any moment — co-viewing across devices
	// still works — while the household itself rotates over time. Without
	// this the co-viewing pair kept ONE fixed IP forever; at 150rps that
	// single household absorbed ~8% of ALL traffic and its freq caps were
	// permanently saturated (the 2026-08-05 "wall of adserver 429s").
	HouseholdShared bool
	// Interests are content-category slugs (matching placement categories,
	// e.g. "dogs") the persona prefers to browse. The simulator's run loop
	// biases placement selection toward matching inventory, so the persona's
	// behaviour signals COHERENTLY earn audience-segment membership via the
	// profile-builder's category rules — a dog person browses dog pages and
	// becomes a dog-lover, rather than being declared one. Empty = no bias.
	Interests []string
	// UserPool > 0 gives the persona a population of STABLE user ids
	// ("{name}-u{n}"): each request draws one from the pool, so the same
	// synthetic users recur across requests and can accumulate the repeat
	// behaviour signals that frequency rules (min_count > 1) require. The
	// mirror path otherwise randomises user_id per request (a freq-cap
	// starvation fix) — which makes earning membership impossible. Sized
	// small enough that a short run revisits each user several times.
	UserPool int
	// Weight is the relative sampling frequency in a mix (higher = more common).
	Weight int
}

// RequestUserID returns a stable pooled user id for ONE request, or "" when
// the persona has no user pool (callers keep their existing id behaviour).
func (p Persona) RequestUserID(rng *rand.Rand) string {
	if p.UserPool <= 0 {
		return ""
	}
	n := 0
	if rng != nil {
		n = rng.Intn(p.UserPool)
	} else {
		n = rand.Intn(p.UserPool)
	}
	return fmt.Sprintf("%s-u%03d", p.Name, n)
}

// RequestIP returns the client IP for ONE request: the persona's fixed
// address, or — with a HouseholdPool — one of pool-many addresses spread
// across the persona's prefix (last two octets vary), each a distinct
// household to the SSP's hh:HMAC(salt,IP) derivation.
func (p Persona) RequestIP(rng *rand.Rand) string {
	if p.IP == "" || p.HouseholdPool <= 1 {
		return p.IP
	}
	var a, b, c, d int
	if _, err := fmt.Sscanf(p.IP, "%d.%d.%d.%d", &a, &b, &c, &d); err != nil {
		return p.IP
	}
	n := 0
	switch {
	case p.HouseholdShared:
		// Time-slotted so every persona sharing this IP+pool derives the
		// same slot right now (co-viewing), rotating households every 10m.
		n = int(time.Now().Unix()/600) % p.HouseholdPool
	case rng != nil:
		n = rng.Intn(p.HouseholdPool)
	default:
		n = rand.Intn(p.HouseholdPool) // query-param path has no seeded rng
	}
	return fmt.Sprintf("%d.%d.%d.%d", a, b, (c+n/254)%256, 1+(d+n)%254)
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
		Segments: []string{"in_market_auto", "sports_enthusiast"}, HouseholdPool: 16384, IP: "203.0.113.10", Weight: 24,
	},
	{
		Name: "us-personalised-desktop", Regime: RegimeUSClear, Identity: IdentityHashedEmail,
		Geo: "USA", Region: "CA", City: "San Francisco", Device: "desktop", OS: "macOS", Make: "Apple",
		Segments: []string{"finance_intender", "high_income"}, HouseholdPool: 16384, IP: "203.0.113.11", Weight: 18,
	},
	{
		Name: "us-firstparty-tablet", Regime: RegimeUSClear, Identity: IdentityPublisherID,
		Geo: "USA", Region: "TX", City: "Austin", Device: "tablet", OS: "Android", Make: "Samsung", Model: "SM-T870",
		Segments: []string{"parenting", "in_market_auto"}, HouseholdPool: 16384, IP: "203.0.113.12", Weight: 8,
	},
	{
		Name: "us-ccpa-optout", Regime: RegimeCCPAOptOut, Identity: IdentityPublisherID,
		Geo: "USA", Region: "CA", City: "Los Angeles", Device: "mobile", OS: "Android", Make: "Google", Model: "Pixel 8",
		Segments: []string{"sports_enthusiast"}, HouseholdPool: 16384, IP: "203.0.113.13", Weight: 6,
	},
	{
		Name: "us-gpc-optout", Regime: RegimeGPC, Identity: IdentityHashedEmail,
		Geo: "USA", Region: "WA", City: "Seattle", Device: "desktop", OS: "Windows",
		Segments: []string{"tech_early_adopter"}, HouseholdPool: 16384, IP: "203.0.113.14", Weight: 4,
	},
	{
		Name: "us-gpp-optout", Regime: RegimeGPPOptOut, Identity: IdentityPublisherID,
		Geo: "USA", Region: "IL", City: "Chicago", Device: "mobile", OS: "iOS", Make: "Apple", Model: "iPhone14,5",
		Segments: []string{"travel_intender"}, HouseholdPool: 16384, IP: "203.0.113.15", Weight: 3,
	},
	{
		Name: "eu-consented-mobile", Regime: RegimeGDPRConsented, Identity: IdentityUID2,
		Geo: "DEU", Region: "BE", City: "Berlin", Device: "mobile", OS: "Android", Make: "Samsung", Model: "SM-S911B",
		Segments: []string{"in_market_auto", "luxury_goods"}, HouseholdPool: 16384, IP: "203.0.113.16", Weight: 10,
	},
	{
		Name: "eu-noconsent-desktop", Regime: RegimeGDPRNoConsent, Identity: IdentityAnonymous,
		Geo: "FRA", Region: "IDF", City: "Paris", Device: "desktop", OS: "Windows", HouseholdPool: 16384, IP: "203.0.113.17", Weight: 8,
	},
	{
		Name: "uk-consented-desktop", Regime: RegimeGDPRConsented, Identity: IdentityHashedEmail,
		Geo: "GBR", Region: "ENG", City: "London", Device: "desktop", OS: "macOS", Make: "Apple",
		Segments: []string{"finance_intender", "travel_intender"}, HouseholdPool: 16384, IP: "203.0.113.18", Weight: 9,
	},
	{
		Name: "coppa-kids-tablet", Regime: RegimeCOPPA, Identity: IdentityAnonymous,
		Geo: "USA", Region: "FL", City: "Miami", Device: "tablet", OS: "iPadOS", Make: "Apple", Model: "iPad13,1", HouseholdPool: 16384, IP: "203.0.113.19", Weight: 2,
	},
	{
		// Shares IP+pool+HouseholdShared with us-ctv-household-mobile: at any
		// moment both derive the SAME household (co-viewing), and the pair
		// rotates through 512 households over time instead of pinning one
		// forever (which saturated that household's caps at load rates).
		Name: "us-ctv-household", Regime: RegimeUSClear, Identity: IdentityPublisherID,
		Geo: "USA", Region: "GA", City: "Atlanta", Device: "ctv", OS: "Roku", Make: "Roku", Model: "Ultra",
		Segments: []string{"sports_enthusiast", "streaming_subscriber"}, IP: "203.0.113.20", HouseholdPool: 512, HouseholdShared: true, Weight: 2,
	},
	{
		// Same IP as us-ctv-household: a second viewer in the SAME household
		// (mobile in the living room) — exercises household-level targeting
		// reaching a different device/user through the shared household id.
		Name: "us-ctv-household-mobile", Regime: RegimeUSClear, Identity: IdentityUID2,
		Geo: "USA", Region: "GA", City: "Atlanta", Device: "mobile", OS: "iOS", Make: "Apple", Model: "iPhone15,2",
		Segments: []string{"streaming_subscriber"}, IP: "203.0.113.20", HouseholdPool: 512, HouseholdShared: true, Weight: 1,
	},
	{
		Name: "anon-mobile-open", Regime: RegimeUSClear, Identity: IdentityAnonymous,
		Geo: "USA", Region: "OH", City: "Columbus", Device: "mobile", OS: "Android", Make: "Motorola", HouseholdPool: 16384, IP: "203.0.113.21", Weight: 6,
	},
}

// ThemedPersonas is the READABLE-WORLD registry (operator design decision,
// 2026-08-07): personas whose browsing behaviour coherently EARNS themed
// audience-segment membership. They are deliberately NOT in the default
// Personas registry — the perf baselines and load profiles keep their exact
// persona mix — and are reached via the "themed" profile (or --persona).
//
// Every themed persona has: Interests matching the themed publishers'
// placement categories (profiles/publishers/themed.yaml), a small UserPool so
// repeat visits accumulate per-user behaviour signals past the seeded rules'
// min_count, and a consent regime that permits behaviour capture — except the
// GPC persona, which browses cat pages forever without ever becoming a
// cat-lover (the consent gate, verifiable in English).
var ThemedPersonas = []Persona{
	{
		Name: "dog-lover-mobile", Regime: RegimeUSClear, Identity: IdentityPublisherID,
		Geo: "USA", Region: "CO", City: "Denver", Device: "mobile", OS: "Android", Make: "Google", Model: "Pixel 8",
		Interests: []string{"dogs"}, UserPool: 40, HouseholdPool: 4096, IP: "203.0.113.30", Weight: 10,
	},
	{
		Name: "dog-lover-desktop", Regime: RegimeUSClear, Identity: IdentityPublisherID,
		Geo: "GBR", Region: "ENG", City: "Bristol", Device: "desktop", OS: "Windows",
		Interests: []string{"dogs"}, UserPool: 30, HouseholdPool: 4096, IP: "203.0.113.31", Weight: 6,
	},
	{
		Name: "cat-lover-mobile", Regime: RegimeUSClear, Identity: IdentityPublisherID,
		Geo: "USA", Region: "OR", City: "Portland", Device: "mobile", OS: "iOS", Make: "Apple", Model: "iPhone15,3",
		Interests: []string{"cats"}, UserPool: 40, HouseholdPool: 4096, IP: "203.0.113.32", Weight: 10,
	},
	{
		Name: "cat-lover-desktop", Regime: RegimeGDPRConsented, Identity: IdentityPublisherID,
		Geo: "DEU", Region: "BY", City: "Munich", Device: "desktop", OS: "macOS", Make: "Apple",
		Interests: []string{"cats"}, UserPool: 30, HouseholdPool: 4096, IP: "203.0.113.33", Weight: 6,
	},
	{
		Name: "coffee-snob-desktop", Regime: RegimeUSClear, Identity: IdentityPublisherID,
		Geo: "USA", Region: "WA", City: "Seattle", Device: "desktop", OS: "macOS", Make: "Apple",
		Interests: []string{"coffee"}, UserPool: 30, HouseholdPool: 4096, IP: "203.0.113.34", Weight: 8,
	},
	{
		Name: "fitness-fan-mobile", Regime: RegimeUSClear, Identity: IdentityPublisherID,
		Geo: "USA", Region: "CA", City: "San Diego", Device: "mobile", OS: "iOS", Make: "Apple", Model: "iPhone15,2",
		Interests: []string{"fitness"}, UserPool: 30, HouseholdPool: 4096, IP: "203.0.113.35", Weight: 8,
	},
	{
		// The crossover: a dog person who also browses coffee content —
		// feeds the seeded COMPOSITE segment (dog-lovers AND coffee-browsers).
		Name: "dog-cafe-regular", Regime: RegimeUSClear, Identity: IdentityPublisherID,
		Geo: "USA", Region: "CO", City: "Boulder", Device: "mobile", OS: "Android", Make: "Samsung", Model: "SM-S911B",
		Interests: []string{"dogs", "coffee"}, UserPool: 20, HouseholdPool: 4096, IP: "203.0.113.36", Weight: 4,
	},
	{
		// Browses cat pages under GPC: behaviour capture is consent-blocked,
		// so these users must NEVER appear in cat-lovers — the negative case.
		Name: "cat-lover-gpc", Regime: RegimeGPC, Identity: IdentityPublisherID,
		Geo: "USA", Region: "MN", City: "Minneapolis", Device: "desktop", OS: "Windows",
		Interests: []string{"cats"}, UserPool: 20, HouseholdPool: 4096, IP: "203.0.113.37", Weight: 3,
	},
}

// PersonaByName returns the persona with the given name — searching the
// default registry first, then the themed registry — and whether it was found.
func PersonaByName(name string) (Persona, bool) {
	for _, p := range Personas {
		if p.Name == name {
			return p, true
		}
	}
	for _, p := range ThemedPersonas {
		if p.Name == name {
			return p, true
		}
	}
	return Persona{}, false
}

// PersonaNames returns every registered persona name (default + themed), for
// CLI listing/validation.
func PersonaNames() []string {
	names := make([]string, 0, len(Personas)+len(ThemedPersonas))
	for _, p := range Personas {
		names = append(names, p.Name)
	}
	for _, p := range ThemedPersonas {
		names = append(names, p.Name)
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
