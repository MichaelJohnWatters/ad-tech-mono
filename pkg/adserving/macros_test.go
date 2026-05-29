package adserving

import (
	"strings"
	"testing"
)

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
