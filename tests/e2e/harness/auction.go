//go:build e2e

package harness

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// AuctionResult is the parsed response from the SSP /v1/ssp/request endpoint.
// The SSP wraps the exchange's OpenRTB BidResponse with placement context.
type AuctionResult struct {
	TraceID     string          `json:"trace_id"`
	PlacementID string          `json:"placement_id"`
	PublisherID string          `json:"publisher_id"`
	SiteDomain  string          `json:"site_domain"`
	Width       int             `json:"width"`
	Height      int             `json:"height"`
	BidResponse json.RawMessage `json:"bid_response"`
}

// RunAuction drives an auction through SSP → Exchange → DSPs as if a real
// publisher page requested an ad. Returns the parsed response so subtests
// can assert on winning DSP, clearing price, deal_id.
//
// placementExternalID is the friendly YAML/test key (e.g. "pl-news-mpu"),
// not the UUID — the SSP service does the derivation server-side.
func (h *Harness) RunAuction(t *testing.T, placementExternalID, geo, device, userID string) AuctionResult {
	t.Helper()

	q := []string{}
	add := func(k, v string) {
		if v != "" {
			q = append(q, k+"="+v)
		}
	}
	add("placement_id", placementExternalID)
	add("geo", geo)
	add("device", device)
	add("user_id", userID)

	url := h.URLs.SSP + "/v1/ssp/request"
	if len(q) > 0 {
		url += "?" + strings.Join(q, "&")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("auction request: %v", err)
	}
	resp, err := h.HTTP.Do(req)
	if err != nil {
		t.Fatalf("auction call failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("auction status %d: %s", resp.StatusCode, string(body))
	}
	var out AuctionResult
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("auction decode: %v\nbody: %s", err, string(body))
	}
	return out
}

// BidResponseWinner extracts the winning bid from a SSP-wrapped BidResponse.
// Returns the seat (advertiser/account UUID), clearing price, deal_id, and
// whether NoBid was set. Helper because the JSON shape is verbose to assert
// inline in every test step.
type BidResponseWinner struct {
	NoBid         bool
	Seat          string
	Price         float64
	CampaignID    string
	CreativeID    string
	DealID        string
}

// ExtractWinner parses a BidResponse from an AuctionResult and pulls the
// fields tests usually care about. Doesn't fail the test on NoBid — the
// caller decides whether NoBid is expected for the scenario.
func (h *Harness) ExtractWinner(t *testing.T, r AuctionResult) BidResponseWinner {
	t.Helper()
	var br struct {
		ID      string `json:"id"`
		NoBid   bool   `json:"nobid,omitempty"`
		SeatBid []struct {
			Seat string `json:"seat"`
			Bid  []struct {
				ID    string  `json:"id"`
				Price float64 `json:"price"`
				CID   string  `json:"cid"`
				CrID  string  `json:"crid"`
				DealID string `json:"dealid,omitempty"`
			} `json:"bid"`
		} `json:"seatbid"`
	}
	if err := json.Unmarshal(r.BidResponse, &br); err != nil {
		t.Fatalf("decode bid response: %v\nraw: %s", err, string(r.BidResponse))
	}
	if br.NoBid || len(br.SeatBid) == 0 || len(br.SeatBid[0].Bid) == 0 {
		return BidResponseWinner{NoBid: true}
	}
	sb := br.SeatBid[0]
	b := sb.Bid[0]
	return BidResponseWinner{
		Seat: sb.Seat, Price: b.Price,
		CampaignID: b.CID, CreativeID: b.CrID, DealID: b.DealID,
	}
}
