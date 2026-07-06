//go:build e2e

package harness

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	neturl "net/url"
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

// AuctionParams carries the optional bid-signal query params the SSP stamps
// into the OpenRTB request. Empty fields are omitted.
type AuctionParams struct {
	Placement string // friendly external key, not the UUID
	Geo       string // ISO country (Device.geo.country)
	Device    string // device type keyword
	UserID    string // User.id (drives segment lookup)
	UID2      string // Unified ID 2.0 token → User.eids (cookieless identity)
	OS        string // Device.os — for OS targeting
	Keywords  string // Site.keywords (comma-separated) — for keyword targeting
	Segments  string // User.ext.segments (comma-separated) — for segment targeting
	// Privacy signals the SSP stamps into Regs / User.ext.consent. Let privacy
	// tests exercise the consent path end-to-end (SSP → exchange → DSP).
	GDPR      string // "1" sets Regs.ext.gdpr
	Consent   string // TCF consent string → User.ext.consent
	USPrivacy string // IAB US Privacy string → Regs.ext.us_privacy
	COPPA     string // "1" sets Regs.coppa
	GPP       string // GPP string → Regs.ext.gpp
	GPPSID    string // GPP section ids → Regs.ext.gpp_sid
	GPC       string // "1" sets Regs.ext.gpc (Global Privacy Control)
}

// RunAuction drives an auction through SSP → Exchange → DSPs as if a real
// publisher page requested an ad. Returns the parsed response so subtests
// can assert on winning DSP, clearing price, deal_id.
//
// placementExternalID is the friendly YAML/test key (e.g. "pl-news-mpu"),
// not the UUID — the SSP service does the derivation server-side.
func (h *Harness) RunAuction(t *testing.T, placementExternalID, geo, device, userID string) AuctionResult {
	return h.RunAuctionWith(t, AuctionParams{Placement: placementExternalID, Geo: geo, Device: device, UserID: userID})
}

// RunAuctionWith is RunAuction with the full optional bid-signal set (OS,
// keywords). Existing callers use RunAuction; targeting tests use this.
func (h *Harness) RunAuctionWith(t *testing.T, p AuctionParams) AuctionResult {
	t.Helper()

	vals := neturl.Values{}
	add := func(k, v string) {
		if v != "" {
			vals.Set(k, v)
		}
	}
	add("placement_id", p.Placement)
	add("geo", p.Geo)
	add("device", p.Device)
	add("user_id", p.UserID)
	add("uid2", p.UID2)
	add("os", p.OS)
	add("keywords", p.Keywords)
	add("segments", p.Segments)
	add("gdpr", p.GDPR)
	add("gdpr_consent", p.Consent)
	add("us_privacy", p.USPrivacy)
	add("coppa", p.COPPA)
	add("gpp", p.GPP)
	add("gpp_sid", p.GPPSID)
	add("gpc", p.GPC)

	url := h.URLs.SSP + "/v1/ssp/request"
	if len(vals) > 0 {
		url += "?" + vals.Encode()
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
	NoBid      bool
	Seat       string
	Price      float64
	CampaignID string
	CreativeID string
	DealID     string
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
				ID     string  `json:"id"`
				Price  float64 `json:"price"`
				CID    string  `json:"cid"`
				CrID   string  `json:"crid"`
				DealID string  `json:"dealid,omitempty"`
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
