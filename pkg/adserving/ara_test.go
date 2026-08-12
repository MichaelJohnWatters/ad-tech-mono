package adserving

import (
	"net/url"
	"strings"
	"testing"
)

func TestAraDestination(t *testing.T) {
	cases := map[string]string{
		"https://shop.acme.co.uk/p?x=1": "https://acme.co.uk",
		"https://www.example.com/":      "https://example.com",
		"http://example.com":            "http://example.com",
		"https://localhost:9200/x":      "https://localhost", // no public suffix → bare host
		"acme.com/deals":                "https://acme.com",  // scheme-less → https
		"":                              "",
		"not a url":                     "",
	}
	for in, want := range cases {
		if got := araDestination(in); got != want {
			t.Errorf("araDestination(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildARASourceURL(t *testing.T) {
	base := MacroContext{
		AdvertiserID: "adv-1",
		CampaignID:   "li-1",
		LandingURL:   "https://www.acme.com/deals",
		TrackerURL:   "https://tracker.example",
	}

	got := BuildARASourceURL(base)
	if got == "" {
		t.Fatal("expected a source URL")
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !strings.HasSuffix(u.Path, "/v1/t/ara/src") {
		t.Errorf("path = %q, want /v1/t/ara/src", u.Path)
	}
	q := u.Query()
	if q.Get("advid") != "adv-1" {
		t.Errorf("advid = %q", q.Get("advid"))
	}
	if q.Get("dest") != "https://acme.com" {
		t.Errorf("dest = %q, want https://acme.com (registrable domain)", q.Get("dest"))
	}
	if q.Get("sig") == "" {
		t.Error("source URL must be signed (sig param present)")
	}

	// No landing URL (no destination) or no advertiser → no beacon.
	if BuildARASourceURL(MacroContext{AdvertiserID: "adv-1", TrackerURL: "https://t"}) != "" {
		t.Error("expected empty URL with no landing URL")
	}
	if BuildARASourceURL(MacroContext{LandingURL: "https://acme.com", TrackerURL: "https://t"}) != "" {
		t.Error("expected empty URL with no advertiser")
	}
}
