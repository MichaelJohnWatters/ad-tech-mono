package request

import (
	"encoding/json"
	"math/rand"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/native"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/privacy"
)

func testInput(ch Channel, p Persona) Input {
	return Input{
		TraceID: "trace-abc",
		Channel: ch,
		Persona: p,
		Placement: Placement{
			Domain: "sim-news.example", PublisherID: "pub-uuid-1", TagID: "plc-uuid-1",
			Categories: []string{"IAB1"}, Keywords: "tech,ai", BidFloor: 1.25,
		},
		Rand: rand.New(rand.NewSource(42)),
	}
}

// TestBuildPopulatesConsumedFields asserts every field a platform service reads
// is present — the whole point of "production-like".
func TestBuildPopulatesConsumedFields(t *testing.T) {
	p, _ := PersonaByName("us-personalised-mobile")
	req := Build(testInput(Display, p))

	if req.ID != "trace-abc" {
		t.Fatalf("id = %q", req.ID)
	}
	if len(req.Imp) != 1 {
		t.Fatalf("want 1 imp, got %d", len(req.Imp))
	}
	imp := req.Imp[0]
	if imp.TagID != "plc-uuid-1" {
		t.Errorf("TagID = %q, want placement uuid", imp.TagID)
	}
	if imp.BidFloor != 1.25 {
		t.Errorf("BidFloor = %v", imp.BidFloor)
	}
	if imp.Banner == nil || imp.Banner.W == 0 {
		t.Errorf("display imp missing banner")
	}
	if imp.Ext == nil || imp.Ext.Channel != "display" {
		t.Errorf("imp.ext.channel not set")
	}
	if req.Site == nil || req.Site.Publisher == nil || req.Site.Publisher.ID != "pub-uuid-1" {
		t.Errorf("site.publisher.id missing (deal matching needs it)")
	}
	if req.Site.Domain == "" || len(req.Site.Cat) == 0 || req.Site.Keywords == "" {
		t.Errorf("contextual signals missing: %+v", req.Site)
	}
	if req.Device == nil || req.Device.Geo == nil || req.Device.Geo.Country != "USA" {
		t.Errorf("device geo missing")
	}
	if req.Device.UA == "" || req.Device.IP == "" || req.Device.OS == "" {
		t.Errorf("device UA/IP/OS missing (fraud + identity read these)")
	}
	if req.TMax == 0 {
		t.Errorf("tmax not set")
	}

	// Supply chain must validate.
	sc := openrtb.SChainOf(&req)
	if err := openrtb.ValidateSChain(sc); err != nil {
		t.Errorf("schain invalid: %v", err)
	}
}

// TestChannelImp asserts each channel attaches the right impression object.
func TestChannelImp(t *testing.T) {
	p, _ := PersonaByName("us-personalised-mobile")
	for _, tc := range []struct {
		ch     Channel
		assert func(openrtb.Imp) bool
	}{
		{Display, func(i openrtb.Imp) bool { return i.Banner != nil && i.Video == nil }},
		{Video, func(i openrtb.Imp) bool { return i.Video != nil && i.Video.MaxDuration > 0 }},
		{Audio, func(i openrtb.Imp) bool { return i.Audio != nil && i.Audio.Feed == 2 }},
		{Native, func(i openrtb.Imp) bool { return i.Native != nil && i.Native.Request != "" }},
	} {
		req := Build(testInput(tc.ch, p))
		if !tc.assert(req.Imp[0]) {
			t.Errorf("channel %s produced wrong imp: %+v", tc.ch, req.Imp[0])
		}
	}
}

// TestNativeRequestParses asserts the embedded native request is valid JSON the
// native package can round-trip.
func TestNativeRequestParses(t *testing.T) {
	p, _ := PersonaByName("us-personalised-mobile")
	req := Build(testInput(Native, p))
	var nr native.Request
	if err := json.Unmarshal([]byte(req.Imp[0].Native.Request), &nr); err != nil {
		t.Fatalf("native request not valid JSON: %v", err)
	}
	if len(nr.Assets) == 0 {
		t.Errorf("native request has no assets")
	}
}

// TestRegimesDrivePrivacyDecision is the key production-like check: each regime
// must produce OpenRTB signals that pkg/privacy.Evaluate resolves to the
// intended verdict, proving the simulator exercises every privacy branch.
func TestRegimesDrivePrivacyDecision(t *testing.T) {
	cases := []struct {
		regime      Regime
		wantBid     bool
		wantPersona bool // Personalise
	}{
		{RegimeUSClear, true, true},
		{RegimeGDPRConsented, true, true},
		{RegimeGDPRNoConsent, true, false},
		{RegimeCCPAOptOut, true, false},
		{RegimeGPC, true, false},
		{RegimeCOPPA, true, false},
		{RegimeGPPOptOut, true, false},
	}
	for _, c := range cases {
		p := Persona{Name: "x", Regime: c.regime, Identity: IdentityPublisherID, Geo: "USA", Device: "mobile"}
		req := Build(Input{TraceID: "t", Persona: p, Rand: rand.New(rand.NewSource(1))})
		sig := signalsFromRequest(&req)
		got := privacy.Evaluate(sig)
		if got.Bid != c.wantBid || got.Personalise != c.wantPersona {
			t.Errorf("regime %s: got {bid:%v personalise:%v reason:%s}, want {bid:%v personalise:%v}",
				c.regime, got.Bid, got.Personalise, got.Reason, c.wantBid, c.wantPersona)
		}
	}
}

// signalsFromRequest extracts privacy.Signals from a built request the same way
// the DSP bid handler does, so the test checks the real decision path.
func signalsFromRequest(req *openrtb.BidRequest) privacy.Signals {
	s := privacy.Signals{}
	if req.Regs != nil {
		s.COPPA = req.Regs.COPPA
		if req.Regs.Ext != nil {
			s.GDPR = req.Regs.Ext.GDPR
			s.USPrivacy = req.Regs.Ext.USPrivacy
			s.GPP = req.Regs.Ext.GPP
			s.GPPSID = req.Regs.Ext.GPPSID
			s.GPC = req.Regs.Ext.GPC == 1
		}
	}
	if req.User != nil && req.User.Ext != nil {
		s.TCFConsent = req.User.Ext.Consent
	}
	return s
}

// TestIdentityTypes asserts each identity type sets the expected signal and
// that UserKey resolves for addressable personas.
func TestIdentityTypes(t *testing.T) {
	mk := func(id Identity) *openrtb.User {
		p := Persona{Name: "x", Regime: RegimeUSClear, Identity: id, Geo: "USA", Device: "mobile"}
		req := Build(Input{TraceID: "t", Persona: p, Rand: rand.New(rand.NewSource(7))})
		return req.User
	}
	if u := mk(IdentityPublisherID); openrtb.UserKey(u) == "" {
		t.Error("publisher_id: UserKey empty")
	}
	if u := mk(IdentityUID2); openrtb.UID2From(u) == "" {
		t.Error("uid2: no UID2 token")
	}
	if u := mk(IdentityHashedEmail); u.Ext == nil || len(u.Ext.HashedEmail) != 64 {
		t.Error("hashed_email: not a 64-hex hash")
	}
	if u := mk(IdentityAnonymous); openrtb.UserKey(u) != "" {
		t.Error("anonymous: should have no user key")
	}
}

// TestPickWeighted asserts weighted sampling favours higher-weight personas.
func TestPickWeighted(t *testing.T) {
	rng := rand.New(rand.NewSource(99))
	personas := []Persona{{Name: "heavy", Weight: 90}, {Name: "light", Weight: 10}}
	counts := map[string]int{}
	for i := 0; i < 1000; i++ {
		counts[Pick(rng, personas).Name]++
	}
	if counts["heavy"] <= counts["light"] {
		t.Errorf("weighting off: %+v", counts)
	}
}

// TestQueryParamsMirrorBuild asserts the browser-path params carry the same
// regime/identity signals as the OpenRTB builder.
func TestQueryParamsMirrorBuild(t *testing.T) {
	p, _ := PersonaByName("eu-consented-mobile")
	v := p.QueryParams("pl-ext-1", Video)
	if v.Get(ParamGDPR) != "1" || v.Get(ParamConsent) == "" {
		t.Errorf("consented EU persona missing gdpr/consent params: %v", v)
	}
	if v.Get(ParamChannel) != "video" || v.Get(ParamGeo) != "DEU" {
		t.Errorf("channel/geo params wrong: %v", v)
	}
	if v.Get(ParamUID2) == "" {
		t.Errorf("uid2 persona missing uid2 param")
	}
}
