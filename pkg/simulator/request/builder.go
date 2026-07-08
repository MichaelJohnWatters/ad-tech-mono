package request

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/rand"
	"strings"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/native"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
)

// Placement describes where the ad runs — the publisher/site, the specific
// placement, and its format constraints. These fields drive deal matching
// (PublisherID + TagID), contextual/keyword targeting (Categories + Keywords),
// and creative-size/duration matching on the DSP.
type Placement struct {
	Domain      string   // publisher domain, e.g. "sim-news.example"
	Name        string   // human name of the site/app
	Page        string   // full page URL (defaults to https://<domain>/... )
	PublisherID string   // publisher account UUID — deal eligibility keys on this
	TagID       string   // placement UUID — deal placement allowlist keys on this
	Categories  []string // IAB content categories for the page
	Keywords    string   // comma-separated page keywords
	BidFloor    float64  // placement floor in USD CPM
	Inventory   string   // "site" (default) or "app"
	Bundle      string   // app bundle id (when Inventory=="app")
	Width       int      // display/companion width (default 300)
	Height      int      // display/companion height (default 250)
	MinDuration int      // video/audio min duration seconds (default 5)
	MaxDuration int      // video/audio max duration seconds (default 30)
}

// Seller identifies the SSP node this request originates from, used to build a
// complete one-hop supply chain (the exchange validates source.ext.schain).
type Seller struct {
	ASI string // seller system domain (matches sellers.json host)
	SID string // seller account id within that system
}

// DefaultSeller is the simulator's stand-in SSP for the supply chain.
var DefaultSeller = Seller{ASI: "sim-ssp.adtech.local", SID: "sim-seller-01"}

// Input is everything Build needs to produce one bid request.
type Input struct {
	TraceID   string    // becomes BidRequest.ID; keep in lockstep with traceparent
	Channel   Channel   // display/video/audio/native (defaults to Display)
	Persona   Persona   // who/where/consent
	Placement Placement // where the ad runs
	Seller    Seller    // supply-chain origin (defaults to DefaultSeller)
	TMax      int       // max response time ms (default 120)
	// Pod, when >1 on a Video/Audio channel, marks the impression as one ad
	// pod slot: it sets the OpenRTB pod fields (PodID, PodSeq, RqdDurs) so the
	// request advertises a CTV/long-form ad pod of that many slots. Ignored for
	// display/native.
	Pod  int
	Rand *rand.Rand
}

// Build assembles a production-like openrtb.BidRequest from an Input. Every
// field a platform service reads is populated: supply chain, publisher/placement
// ids, contextual signals, device, identity, audience segments, privacy/regs,
// and a channel-appropriate impression. Optional fields stay unset so the JSON
// stays close to what a real SSP emits.
func Build(in Input) openrtb.BidRequest {
	rng := in.Rand
	if rng == nil {
		rng = rand.New(rand.NewSource(1))
	}
	ch := in.Channel
	if ch == "" {
		ch = Display
	}
	seller := in.Seller
	if seller.ASI == "" {
		seller = DefaultSeller
	}
	tmax := in.TMax
	if tmax == 0 {
		tmax = 120
	}
	pl := withPlacementDefaults(in.Placement)
	p := in.Persona

	req := openrtb.BidRequest{
		ID:     in.TraceID,
		Imp:    []openrtb.Imp{buildImp(ch, pl, rng)},
		Device: buildDevice(p, rng),
		User:   buildUser(p, rng),
		Regs:   buildRegs(p),
		Source: buildSource(seller, in.TraceID),
		TMax:   tmax,
		Cur:    []string{"USD"},
	}

	// Ad pod: advertise a pod of Pod slots on the video/audio imp so CTV /
	// long-form breaks are modelled (PodID shared across slots, RqdDurs one
	// required duration per slot at the placement's max duration).
	if in.Pod > 1 {
		podID := "pod-" + in.TraceID
		durs := make([]int, in.Pod)
		for i := range durs {
			durs[i] = pl.MaxDuration
		}
		if v := req.Imp[0].Video; v != nil {
			v.PodID, v.PodSeq, v.RqdDurs = podID, 1, durs
		}
		if a := req.Imp[0].Audio; a != nil {
			a.PodID, a.PodSeq, a.RqdDurs, a.MaxSeq = podID, 1, durs, in.Pod
		}
	}

	inv := pl.Inventory
	if inv == "app" {
		req.App = &openrtb.App{
			Bundle: pl.Bundle, Name: pl.Name, Cat: pl.Categories,
			Publisher: &openrtb.Publisher{ID: pl.PublisherID, Name: pl.Name},
		}
	} else {
		req.Site = &openrtb.Site{
			Domain: pl.Domain, Name: pl.Name, Page: pl.Page,
			Cat: pl.Categories, Keywords: pl.Keywords,
			Publisher: &openrtb.Publisher{ID: pl.PublisherID, Name: pl.Name},
		}
	}
	return req
}

func withPlacementDefaults(pl Placement) Placement {
	if pl.Domain == "" {
		pl.Domain = "sim-publisher.example"
	}
	if pl.Name == "" {
		pl.Name = pl.Domain
	}
	if pl.Page == "" {
		pl.Page = "https://" + pl.Domain + "/article"
	}
	if len(pl.Categories) == 0 {
		pl.Categories = []string{"IAB17"} // sports, a safe default
	}
	if pl.Keywords == "" {
		pl.Keywords = "news,sports,headlines"
	}
	if pl.BidFloor == 0 {
		pl.BidFloor = 0.50
	}
	if pl.Inventory == "" {
		pl.Inventory = "site"
	}
	if pl.Width == 0 {
		pl.Width = 300
	}
	if pl.Height == 0 {
		pl.Height = 250
	}
	if pl.MinDuration == 0 {
		pl.MinDuration = 5
	}
	if pl.MaxDuration == 0 {
		pl.MaxDuration = 30
	}
	return pl
}

// buildImp attaches the format object for the requested channel. TagID carries
// the placement UUID (deal matching); BidFloor + ImpExt.channel are always set.
func buildImp(ch Channel, pl Placement, rng *rand.Rand) openrtb.Imp {
	imp := openrtb.Imp{
		ID:          "1",
		TagID:       pl.TagID,
		BidFloor:    pl.BidFloor,
		BidFloorCur: "USD",
		Ext:         &openrtb.ImpExt{Channel: string(ch)},
	}
	switch ch {
	case Video:
		imp.Video = &openrtb.Video{
			Mimes:          []string{"video/mp4", "video/webm"},
			Protocols:      []int{2, 3, 5, 6, 7}, // VAST 2.0–4.2
			W:              640,
			H:              360,
			MinDuration:    pl.MinDuration,
			MaxDuration:    pl.MaxDuration,
			Linearity:      1, // linear (pre/mid/post-roll)
			Plcmt:          1, // instream with audio
			Pos:            7, // fullscreen
			StartDelay:     0, // pre-roll
			Skip:           1,
			SkipAfter:      5,
			PlaybackMethod: []int{2}, // autoplay, sound off
			Delivery:       []int{2}, // progressive
			API:            []int{7}, // OMID 1.0
		}
	case Audio:
		imp.Audio = &openrtb.Audio{
			Mimes:       []string{"audio/mp4", "audio/mpeg", "audio/aac"},
			Protocols:   []int{1, 2, 9}, // DAAST 1.0, wrapper, VAST 3.0 audio ext
			MinDuration: pl.MinDuration,
			MaxDuration: pl.MaxDuration,
			StartDelay:  0,        // pre-roll
			Delivery:    []int{2}, // progressive
			Feed:        2,        // podcast
			Stitched:    0,        // client-side insertion
			NVol:        3,        // loudness normalised (LUFS)
		}
	case Native:
		reqJSON, err := native.MarshalRequest(native.StandardRequest(native.Spec{
			WantIcon: true, WantBody: true, WantCTA: true,
		}))
		if err == nil {
			imp.Native = &openrtb.Native{Request: reqJSON, Ver: native.Ver}
		}
	default: // Display
		imp.Banner = &openrtb.Banner{
			W: pl.Width, H: pl.Height,
			Mimes: []string{"image/jpeg", "image/png", "text/html"},
		}
	}
	return imp
}

// buildDevice sets device type, OS, make/model, a residential-looking IP, and a
// browser-shaped UA (identity fingerprinting + fraud read these).
func buildDevice(p Persona, rng *rand.Rand) *openrtb.Device {
	return &openrtb.Device{
		UA:             userAgent(p),
		IP:             residentialIP(rng),
		DeviceType:     deviceTypeInt(p.Device),
		Make:           p.Make,
		Model:          p.Model,
		OS:             p.OS,
		ConnectionType: 2, // wifi
		Geo:            &openrtb.Geo{Country: p.Geo, Region: p.Region, City: p.City},
	}
}

// buildUser sets the identity signals for the persona's identity type plus any
// audience segments. Anonymous personas get no id at all.
func buildUser(p Persona, rng *rand.Rand) *openrtb.User {
	u := &openrtb.User{}
	ext := &openrtb.UserExt{}
	switch p.Identity {
	case IdentityPublisherID:
		u.ID = fmt.Sprintf("pub-user-%08x", rng.Uint32())
		ext.PublisherUserID = u.ID
	case IdentityUID2:
		u.EIDs = []openrtb.EID{openrtb.UID2EID(uid2Token(rng))}
	case IdentityHashedEmail:
		ext.HashedEmail = hashedEmail(rng)
	case IdentityAnonymous:
		// no identifier
	}
	if len(p.Segments) > 0 {
		ext.Segments = append([]string(nil), p.Segments...)
	}
	applyConsent(p.Regime, ext)
	if !emptyUserExt(ext) {
		u.Ext = ext
	}
	return u
}

// buildRegs sets the regulatory object (COPPA + ext gdpr/us_privacy/gpp/gpc)
// for the persona's privacy regime.
func buildRegs(p Persona) *openrtb.Regs {
	regs := &openrtb.Regs{Ext: &openrtb.RegsExt{}}
	switch p.Regime {
	case RegimeGDPRConsented, RegimeGDPRNoConsent:
		regs.Ext.GDPR = 1
	case RegimeCCPAOptOut:
		regs.Ext.USPrivacy = "1YYN" // notice given, opted out of sale
	case RegimeGPC:
		regs.Ext.GPC = 1
	case RegimeCOPPA:
		regs.COPPA = 1
	case RegimeGPPOptOut:
		regs.Ext.GPP = gppUSNationalOptOut()
		regs.Ext.GPPSID = "7"
	case RegimeUSClear:
		regs.Ext.USPrivacy = "1YNN" // notice given, did NOT opt out
	}
	return regs
}

// applyConsent writes the TCF consent string onto the user ext for consented
// GDPR traffic (the DSP's privacy engine treats a present, non-empty consent
// string as "consented" for our simulator's purposes).
func applyConsent(r Regime, ext *openrtb.UserExt) {
	if r == RegimeGDPRConsented {
		ext.Consent = tcfConsentString
	}
}

func buildSource(seller Seller, traceID string) *openrtb.Source {
	return &openrtb.Source{
		FD:  1, // upstream (SSP) is the seller of record in the chain
		TID: traceID,
		Ext: &openrtb.SourceExt{
			SChain: &openrtb.SupplyChain{
				Complete: 1,
				Ver:      openrtb.SChainVersion,
				Nodes: []openrtb.SupplyChainNode{{
					ASI: seller.ASI, SID: seller.SID, HP: 1,
					RID: traceID, Name: "simulator-ssp", Domain: seller.ASI,
				}},
			},
		},
	}
}

// --- value generators ---------------------------------------------------

// tcfConsentString is a static, well-formed-looking TCF v2 consent string. The
// simulator doesn't need a semantically decoded string — the privacy engine
// only checks presence for the GDPR-consent path — but a realistic value keeps
// logs/traces authentic.
const tcfConsentString = "CPcqAAAPcqAAAAKAxAENCgCsAP_AAH_AAAAAJ1Nf_X__b3_j-_5_f_t0eY1P9_7__-0zjhfdt-8N3f_X_L8X_2M7vB36pq4KuR4Eu3LBIQdlHOHcTUmw6IkVqTPsbk2Mr7NKJ7PEinMbe2dYGH9_n93TuZKY7_____z_v-v_v____f_7-3_3__p9X---_e_V399zLv9____39nP___9v-_9_____4IQAJMNS-AizEscCSaNKoUQ_ZL1QG"

func userAgent(p Persona) string {
	switch p.Device {
	case "ctv":
		return "Roku/DVP-13.0 (13.0.0.4193-46) AdTechMono-CTV"
	case "mobile":
		if p.OS == "iOS" {
			return "Mozilla/5.0 (iPhone; CPU iPhone OS 17_4 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4 Mobile/15E148 Safari/604.1"
		}
		return "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Mobile Safari/537.36"
	case "tablet":
		return "Mozilla/5.0 (iPad; CPU OS 17_4 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4 Safari/604.1"
	default: // desktop
		if p.OS == "macOS" {
			return "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
		}
		return "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
	}
}

// residentialIP returns a random public-looking IPv4 that avoids the obvious
// cloud/datacenter ranges the fraud engine flags, so simulated traffic isn't
// scored as bot/dc by default.
func residentialIP(rng *rand.Rand) string {
	// First octets drawn from consumer ISP allocations (Comcast, BT, Deutsche
	// Telekom, etc.), deliberately not AWS/GCP/Azure ranges.
	firstOctets := []int{24, 47, 71, 76, 81, 86, 92, 109, 151, 174, 188, 217}
	a := firstOctets[rng.Intn(len(firstOctets))]
	return fmt.Sprintf("%d.%d.%d.%d", a, rng.Intn(256), rng.Intn(256), 1+rng.Intn(254))
}

func uid2Token(rng *rand.Rand) string {
	b := make([]byte, 32)
	for i := range b {
		b[i] = byte(rng.Intn(256))
	}
	return base64.StdEncoding.EncodeToString(b)
}

func hashedEmail(rng *rand.Rand) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("user%d@sim.example", rng.Int63())))
	return hex.EncodeToString(sum[:])
}

func deviceTypeInt(device string) int {
	switch device {
	case "mobile":
		return 1
	case "desktop":
		return 2
	case "ctv":
		return 3
	case "tablet":
		return 5
	default:
		return 2
	}
}

func emptyUserExt(e *openrtb.UserExt) bool {
	return e.Consent == "" && e.PublisherUserID == "" && e.HashedEmail == "" &&
		len(e.Segments) == 0 && len(e.Demographics) == 0
}

// gppUSNationalOptOut builds a GPP string with a single US-National (section 7)
// segment encoding a "sale" opt-out, matching the bit layout pkg/privacy.GPP
// decodes. Header "DBABLA" + "~" + base64url(section). Kept in sync with the
// layout documented in pkg/privacy/gpp.go: 6-bit version, six 2-bit notice
// fields, then sale/sharing/targeted 2-bit opt-outs (1 = opted out).
func gppUSNationalOptOut() string {
	w := &bitWriter{}
	w.write(1, 6)  // version
	w.write(0, 12) // six 2-bit notice fields, N/A
	w.write(1, 2)  // sale: opted out
	w.write(2, 2)  // sharing: did not opt out
	w.write(2, 2)  // targeted: did not opt out
	w.write(0, 8)  // trailing padding
	return "DBABLA~" + base64.RawURLEncoding.EncodeToString(w.bytes())
}

// bitWriter packs big-endian bit fields, mirroring the reader in pkg/privacy/gpp.go.
type bitWriter struct {
	buf []byte
	pos int
}

func (w *bitWriter) write(val, bits int) {
	for i := bits - 1; i >= 0; i-- {
		if w.pos%8 == 0 {
			w.buf = append(w.buf, 0)
		}
		if (val>>i)&1 == 1 {
			w.buf[w.pos/8] |= 1 << (7 - (w.pos % 8))
		}
		w.pos++
	}
}

func (w *bitWriter) bytes() []byte { return w.buf }

// Segments returns the persona's segments joined for logging/params.
func (p Persona) SegmentsCSV() string { return strings.Join(p.Segments, ",") }
