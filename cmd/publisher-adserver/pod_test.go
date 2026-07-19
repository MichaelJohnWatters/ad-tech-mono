package main

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/vast"
)

// rotatingSSP returns a stub SSP whose winner advertiser rotates through
// advertisers so a pod request can fill distinct slots. If distinct is false it
// always returns the same advertiser (to exercise competitive separation).
func rotatingSSP(t *testing.T, advertisers []string) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	i := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		adv := advertisers[i%len(advertisers)]
		i++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = encodeJSON(w, sspVideoWinner{
			TraceID:          fmt.Sprintf("trace-%s", adv),
			Channel:          "video",
			CreativeID:       "cr-" + adv,
			CampaignID:       "li-" + adv,
			PlacementID:      "pl-sport-live-preroll",
			PublisherID:      "pub-sport",
			AdvertiserID:     "adv-" + adv,
			AdvertiserDomain: adv + ".example",
			BidModel:         "cpm",
			Currency:         "USD",
			ClearingPrice:    5.0,
			Width:            640,
			Height:           360,
			DurationSeconds:  15,
			MediaURL:         "https://cdn.example/" + adv + ".mp4",
		})
	}))
}

// TestPodVASTFillsDistinctAds asserts ?pod=3 returns a VAST pod of 3 sequenced
// ads from independent auctions.
func TestPodVASTFillsDistinctAds(t *testing.T) {
	ssp := rotatingSSP(t, []string{"acme", "globex", "initech"})
	defer ssp.Close()

	h := vastHandler(nullLogger(), "http://tracker:8083", ssp.URL, noOMID, alwaysStub, noHouseAds)
	req := httptest.NewRequest("GET", "/v1/pubad/video/vast?placement_id=pl-sport-live-preroll&pod=3", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var doc vast.VAST
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("pod VAST not parseable: %v\n%s", err, rec.Body.String())
	}
	if len(doc.Ads) != 3 {
		t.Fatalf("want 3 ads in pod, got %d", len(doc.Ads))
	}
	// Every ad must carry a sequence attribute (players play pods in order).
	seqs := map[int]bool{}
	for _, ad := range doc.Ads {
		if ad.Sequence == 0 {
			t.Errorf("pod ad missing sequence attr: %+v", ad)
		}
		seqs[ad.Sequence] = true
	}
	if len(seqs) != 3 {
		t.Errorf("pod ad sequences not distinct: %v", seqs)
	}
}

// TestPodCompetitiveSeparation asserts an advertiser is never repeated within a
// pod: if the SSP only ever returns one advertiser, the pod fills just one slot.
func TestPodCompetitiveSeparation(t *testing.T) {
	ssp := rotatingSSP(t, []string{"acme"}) // always the same advertiser
	defer ssp.Close()

	h := vastHandler(nullLogger(), "http://tracker:8083", ssp.URL, noOMID, alwaysStub, noHouseAds)
	req := httptest.NewRequest("GET", "/v1/pubad/video/vast?placement_id=pl-sport-live-preroll&pod=4", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	var doc vast.VAST
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("pod VAST not parseable: %v", err)
	}
	if len(doc.Ads) != 1 {
		t.Errorf("competitive separation failed: want 1 ad (same advertiser deduped), got %d", len(doc.Ads))
	}
}

// TestSingleAdNotAPod asserts pod=1 (or absent) still uses the single-ad path.
func TestSingleAdNotAPod(t *testing.T) {
	ssp := rotatingSSP(t, []string{"acme", "globex"})
	defer ssp.Close()

	h := vastHandler(nullLogger(), "http://tracker:8083", ssp.URL, noOMID, alwaysStub, noHouseAds)
	req := httptest.NewRequest("GET", "/v1/pubad/video/vast?placement_id=pl-sport-live-preroll", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	var doc vast.VAST
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("VAST not parseable: %v", err)
	}
	if len(doc.Ads) != 1 {
		t.Errorf("single ad path should return 1 ad, got %d", len(doc.Ads))
	}
	if strings.Count(rec.Body.String(), "<Ad ") != 1 {
		t.Errorf("expected exactly one <Ad>")
	}
}
