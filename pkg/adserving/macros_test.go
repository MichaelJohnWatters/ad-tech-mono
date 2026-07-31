package adserving

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestBeaconHouseholdBaking: the household id is baked onto the impression and
// viewability beacons ONLY alongside a consented uid — never on an unconsented
// beacon — so behaviour_signals can carry the household fallback key.
func TestBeaconHouseholdBaking(t *testing.T) {
	base := MacroContext{
		AuctionID: "trace-1", CampaignID: "li-1", CreativeID: "cr-1",
		PlacementID: "pl-1", PublisherID: "pub-1", AdvertiserID: "adv-1",
		Currency: "USD", TrackerURL: "http://localhost:8083",
	}
	param := func(rawURL, key string) string {
		u, err := url.Parse(rawURL)
		if err != nil {
			t.Fatalf("parse %s: %v", rawURL, err)
		}
		return u.Query().Get(key)
	}

	// Consented (uid present) + household → both uid and hh on impression + view.
	consented := base
	consented.UserID = "user-abc"
	consented.Household = "hh:home1"
	if got := param(BuildImpressionURL(consented), "hh"); got != "hh:home1" {
		t.Errorf("impression hh = %q, want hh:home1", got)
	}
	if got := param(BuildViewabilityURL(consented), "hh"); got != "hh:home1" {
		t.Errorf("view hh = %q, want hh:home1", got)
	}

	// No consent (uid empty) but household known → hh MUST NOT be baked.
	noConsent := base
	noConsent.Household = "hh:home1"
	if got := param(BuildImpressionURL(noConsent), "hh"); got != "" {
		t.Errorf("unconsented impression leaked hh = %q, want empty", got)
	}
	if got := param(BuildViewabilityURL(noConsent), "hh"); got != "" {
		t.Errorf("unconsented view leaked hh = %q, want empty", got)
	}
}

func TestSubstituteMacros(t *testing.T) {
	ctx := MacroContext{
		AuctionID:    "trace-123",
		AuctionPrice: 2.50,
		Currency:     "USD",
		CampaignID:   "li-456",
		CreativeID:   "cr-789",
		PlacementID:  "pl-001",
		PublisherID:  "pub-002",
		AdvertiserID: "adv-003",
		Width:        300,
		Height:       250,
		TrackerURL:   "http://localhost:8083",
	}

	input := `<img src="/imp?price=${AUCTION_PRICE}&campaign=${CAMPAIGN_ID}&creative=${CREATIVE_ID}&w=${WIDTH}&h=${HEIGHT}">`
	result := SubstituteMacros(input, ctx)

	if strings.Contains(result, "${") {
		t.Errorf("unsubstituted macros remain: %s", result)
	}
	if !strings.Contains(result, "price=2.50") {
		t.Errorf("expected price=2.50 in: %s", result)
	}
	if !strings.Contains(result, "campaign=li-456") {
		t.Errorf("expected campaign=li-456 in: %s", result)
	}
	if !strings.Contains(result, "w=300") {
		t.Errorf("expected w=300 in: %s", result)
	}
}

// TestBuildImpressionURL_GeoDeviceChannel guards the data-slippage fix: the
// impression + click beacons must carry geo=/dev=/ch= when the context has them
// (so analytics record geo/device and the right channel instead of empty +
// hardcoded "display"), and must OMIT them when unset — all covered by the HMAC
// since they're added before signing.
func TestBuildImpressionURL_GeoDeviceChannel(t *testing.T) {
	ctx := MacroContext{
		AuctionID: "t1", PlacementID: "pl-1", PublisherID: "pub-1",
		Geo: "USA", Device: "mobile", Channel: "video",
		TrackerURL: "http://tracker:8083",
	}
	for _, b := range []struct {
		name string
		fn   func(MacroContext) string
	}{{"impression", BuildImpressionURL}, {"click", BuildClickURL}} {
		signed := b.fn(ctx)
		u, err := url.Parse(signed)
		if err != nil {
			t.Fatalf("%s: parse: %v", b.name, err)
		}
		if !ValidateSignature(u.Path, u.Query(), DefaultSigningKey) {
			t.Errorf("%s: geo/dev/ch params must be covered by the HMAC (added pre-sign): %s", b.name, signed)
		}
		q := u.Query()
		if q.Get("geo") != "USA" {
			t.Errorf("%s: geo=USA missing: %s", b.name, signed)
		}
		if q.Get("dev") != "mobile" {
			t.Errorf("%s: dev=mobile missing: %s", b.name, signed)
		}
		if q.Get("ch") != "video" {
			t.Errorf("%s: ch=video missing: %s", b.name, signed)
		}
	}

	// Unset geo/device/channel → params omitted (not empty-valued).
	bare := BuildImpressionURL(MacroContext{AuctionID: "t2", TrackerURL: "http://tracker:8083"})
	for _, p := range []string{"geo=", "dev=", "ch="} {
		if strings.Contains(bare, p) {
			t.Errorf("expected no %q when unset: %s", p, bare)
		}
	}
}

func TestBuildImpressionURL(t *testing.T) {
	ctx := MacroContext{
		AuctionID:    "trace-abc",
		AuctionPrice: 3.00,
		Currency:     "GBP",
		CampaignID:   "camp-1",
		CreativeID:   "cr-1",
		PlacementID:  "pl-1",
		PublisherID:  "pub-1",
		AdvertiserID: "adv-1",
		Width:        728,
		Height:       90,
		TrackerURL:   "http://tracker:8083",
	}

	url := BuildImpressionURL(ctx)

	for _, expected := range []string{"tid=trace-abc", "cid=camp-1", "crid=cr-1", "pid=pl-1", "pubid=pub-1", "price=3.0000", "cur=GBP"} {
		if !strings.Contains(url, expected) {
			t.Errorf("expected %s in URL: %s", expected, url)
		}
	}
}

func TestBuildClickURL(t *testing.T) {
	ctx := MacroContext{
		AuctionID:   "trace-abc",
		CampaignID:  "camp-1",
		CreativeID:  "cr-1",
		PlacementID: "pl-1",
		PublisherID: "pub-1",
		TrackerURL:  "http://tracker:8083",
	}

	url := BuildClickURL(ctx)
	if !strings.Contains(url, "/v1/t/click?") {
		t.Errorf("expected click path in: %s", url)
	}
	if !strings.Contains(url, "tid=trace-abc") {
		t.Errorf("expected tid in: %s", url)
	}
	if strings.Contains(url, "redir=") {
		t.Errorf("redir should be absent when LandingURL is empty: %s", url)
	}
}

// Video / audio event URLs route through the typed tracker endpoints
// (/v1/t/video, /v1/t/audio) so downstream consumers pick up
// VideoEvent / AudioEvent on adtech.events.video|audio instead of
// confusing video quartiles with display viewability events. The
// event token is part of the signed param set; replaying with a
// rewritten event invalidates the signature.
func TestBuildVideoEventURL(t *testing.T) {
	ctx := MacroContext{
		AuctionID:   "trace-abc",
		CampaignID:  "li-1",
		CreativeID:  "cr-1",
		PlacementID: "pl-1",
		PublisherID: "pub-1",
		TrackerURL:  "http://tracker:8083",
	}
	signed := BuildVideoEventURL(ctx, "firstQuartile")
	if !strings.Contains(signed, "/v1/t/video?") {
		t.Errorf("URL must route through /v1/t/video, got %s", signed)
	}
	if !strings.Contains(signed, "event=firstQuartile") {
		t.Errorf("URL must carry event=firstQuartile, got %s", signed)
	}
	u, _ := url.Parse(signed)
	if !ValidateSignature(u.Path, u.Query(), DefaultSigningKey) {
		t.Errorf("video event URL did not validate: %s", signed)
	}
	// Tampering with the event token must invalidate the signature.
	q := u.Query()
	q.Set("event", "complete") // rewrite without re-signing
	if ValidateSignature(u.Path, q, DefaultSigningKey) {
		t.Errorf("rewriting event= without re-signing must invalidate (URL=%s)", signed)
	}
}

func TestBuildAudioEventURL(t *testing.T) {
	ctx := MacroContext{
		AuctionID:   "trace-abc",
		CampaignID:  "li-1",
		CreativeID:  "cr-1",
		PlacementID: "pl-1",
		PublisherID: "pub-1",
		TrackerURL:  "http://tracker:8083",
	}
	signed := BuildAudioEventURL(ctx, "complete")
	if !strings.Contains(signed, "/v1/t/audio?") {
		t.Errorf("audio URL must route through /v1/t/audio, got %s", signed)
	}
	if !strings.Contains(signed, "event=complete") {
		t.Errorf("audio URL must carry event=complete, got %s", signed)
	}
}

func TestSignedURLsCarryExpWhenTTLSet(t *testing.T) {
	ctx := MacroContext{
		AuctionID:   "trace-1",
		CampaignID:  "c",
		CreativeID:  "cr",
		PlacementID: "p",
		PublisherID: "pub",
		TrackerURL:  "http://tracker:8083",
		URLTTL:      time.Hour,
	}
	for name, urlFn := range map[string]func(MacroContext) string{
		"imp":   BuildImpressionURL,
		"click": BuildClickURL,
		"view":  BuildViewabilityURL,
	} {
		signed := urlFn(ctx)
		if !strings.Contains(signed, "exp=") {
			t.Errorf("%s: expected exp= in signed URL: %s", name, signed)
		}
		u, err := url.Parse(signed)
		if err != nil {
			t.Fatalf("%s: parse: %v", name, err)
		}
		if !ValidateSignature(u.Path, u.Query(), DefaultSigningKey) {
			t.Errorf("%s: signature must cover exp: %s", name, signed)
		}
	}

	noTTL := ctx
	noTTL.URLTTL = 0
	if strings.Contains(BuildImpressionURL(noTTL), "exp=") {
		t.Errorf("expected no exp when URLTTL=0")
	}
}

func TestBuildClickURLWithLanding(t *testing.T) {
	ctx := MacroContext{
		AuctionID:   "trace-abc",
		CampaignID:  "camp-1",
		CreativeID:  "cr-1",
		PlacementID: "pl-1",
		PublisherID: "pub-1",
		TrackerURL:  "http://tracker:8083",
		LandingURL:  "https://example.com/landing?utm=a&utm2=b",
	}
	signed := BuildClickURL(ctx)
	if !strings.Contains(signed, "redir=https%3A%2F%2Fexample.com%2Flanding") {
		t.Errorf("expected url-encoded redir in signed URL: %s", signed)
	}
	u, err := url.Parse(signed)
	if err != nil {
		t.Fatalf("parse signed url: %v", err)
	}
	if !ValidateSignature(u.Path, u.Query(), DefaultSigningKey) {
		t.Errorf("signature should cover the baked-in redir param: %s", signed)
	}
}

func TestBuildViewabilityURL(t *testing.T) {
	ctx := MacroContext{
		AuctionID:   "trace-abc",
		CampaignID:  "camp-1",
		PlacementID: "pl-1",
		PublisherID: "pub-1",
		TrackerURL:  "http://tracker:8083",
	}

	url := BuildViewabilityURL(ctx)
	if !strings.Contains(url, "/v1/t/view?") {
		t.Errorf("expected view path in: %s", url)
	}
}

func TestValidateSignatureAny_Overlap(t *testing.T) {
	// A URL signed with the OLD (rotating) key still validates while the key set
	// includes both old and new — the overlap window. Once only the new key is
	// present, the old-signed URL is rejected.
	signed := SignURL("http://x/v1/t/imp?tid=t1&cid=c1", "old-key")
	u, err := url.Parse(signed)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	q := u.Query()
	if !ValidateSignatureAny(u.Path, q, []string{"new-key", "old-key"}) {
		t.Error("expected accept under the overlap set [new, old]")
	}
	if ValidateSignatureAny(u.Path, q, []string{"new-key"}) {
		t.Error("expected reject once the old key is dropped from the set")
	}
	if ValidateSignatureAny(u.Path, q, []string{"", ""}) {
		t.Error("empty keys must not validate")
	}
}
