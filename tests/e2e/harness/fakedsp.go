//go:build e2e

package harness

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// FakeDSP is an in-test stand-in for a real DSP service. Lets a test
// register a fake at a real http URL and then point exchange.dsp_endpoints
// at it via SetConfigForPod. Two modes:
//
//	Mode = "broken":  /v1/openrtb/bid returns 500. Used by tests that need
//	                  to prove the exchange tolerates a misbehaving DSP.
//	Mode = "bidder":  /v1/openrtb/bid returns a bid (configurable price).
//	                  Records inbound /v1/openrtb/win and /v1/openrtb/loss
//	                  calls so the test can assert the exchange's
//	                  notification fan-out behaviour.
//
// Lifecycle: t.Cleanup closes the server.
type FakeDSP struct {
	URL  string
	Mode FakeDSPMode

	mu        sync.Mutex
	winCalls  []FakeNotify
	lossCalls []FakeNotify
	bidCalls  int
	bidReqs   []openrtb.BidRequest

	srv *httptest.Server
}

// FakeDSPMode is the dispatch shape for the fake.
type FakeDSPMode string

const (
	FakeDSPBroken FakeDSPMode = "broken"
	FakeDSPBidder FakeDSPMode = "bidder"
)

// FakeNotify is the parsed query of an inbound /win or /loss callback.
// Field names match the query params the exchange sends — see
// sendWinLossNotifications in cmd/exchange/main.go for the source of truth.
type FakeNotify struct {
	BidID         string
	Reason        string
	ClearingPrice string // /loss only
	CampaignID    string
	PlacementID   string
	Price         string // /win only — the price the winner paid
}

// FakeDSPOpts configures the bidder mode.
type FakeDSPOpts struct {
	Mode FakeDSPMode
	// BidPrice is the CPM the bidder mode will return. Only consulted in
	// FakeDSPBidder mode. 0 = silently no-bid (return BidResponse{NoBid: true}).
	BidPrice float64
	// Seat is the advertiser/account UUID returned on the bid. Defaults to
	// "fake-seat" if empty; tests that assert on the seat should set this.
	Seat string
}

// NewFakeDSP spins up an httptest server and registers cleanup. URL is
// the base for the fake (just like a real DSP URL); tests inject it into
// exchange.dsp_endpoints via SetConfigForPod.
func NewFakeDSP(t *testing.T, opts FakeDSPOpts) *FakeDSP {
	t.Helper()
	f := &FakeDSP{Mode: opts.Mode}

	mux := http.NewServeMux()
	mux.HandleFunc(routes.OpenRTBBid, func(w http.ResponseWriter, r *http.Request) {
		// Decode before mode dispatch: the recorded request is assertable in
		// every mode (segtax tests inspect what the exchange fanned out even
		// when the fake never bids).
		var bidReq openrtb.BidRequest
		_ = json.NewDecoder(r.Body).Decode(&bidReq)

		f.mu.Lock()
		f.bidCalls++
		f.bidReqs = append(f.bidReqs, bidReq)
		f.mu.Unlock()

		if f.Mode == FakeDSPBroken {
			http.Error(w, "fake DSP broken", http.StatusInternalServerError)
			return
		}

		seat := opts.Seat
		if seat == "" {
			seat = "fake-seat"
		}

		if opts.BidPrice <= 0 {
			json.NewEncoder(w).Encode(openrtb.BidResponse{ID: bidReq.ID, NoBid: true})
			return
		}
		impID := "imp-1"
		if len(bidReq.Imp) > 0 {
			impID = bidReq.Imp[0].ID
		}
		resp := openrtb.BidResponse{
			ID:  bidReq.ID,
			Cur: "USD",
			SeatBid: []openrtb.SeatBid{{
				Seat: seat,
				Bid: []openrtb.BidObj{{
					ID:    "fake-bid",
					ImpID: impID,
					Price: opts.BidPrice,
					CID:   "fake-campaign",
					CrID:  "fake-creative",
				}},
			}},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc(routes.OpenRTBWin, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f.mu.Lock()
		f.winCalls = append(f.winCalls, FakeNotify{
			BidID:       q.Get("bid_id"),
			Price:       q.Get("price"),
			CampaignID:  q.Get("campaign_id"),
			PlacementID: q.Get("placement_id"),
		})
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc(routes.OpenRTBLoss, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f.mu.Lock()
		f.lossCalls = append(f.lossCalls, FakeNotify{
			BidID:         q.Get("bid_id"),
			Reason:        q.Get("reason"),
			ClearingPrice: q.Get("clearing_price"),
			CampaignID:    q.Get("campaign_id"),
			PlacementID:   q.Get("placement_id"),
		})
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})

	// Best-effort 404 on anything else so unexpected fan-out surfaces in
	// test logs rather than silently 200ing.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})

	// Use HostReachableServer so the URL is callable from inside a k8s
	// pod (DEV_MODE=container) as well as from a host process. In host
	// mode this behaves identically to httptest.NewServer; in pod mode
	// it binds 0.0.0.0 and returns a URL with ADTECH_HOST_IP so the
	// exchange/pubad/etc pod can call back.
	srv := HostReachableServer(mux)
	f.srv = srv
	f.URL = srv.URL
	t.Cleanup(srv.Close)
	return f
}

// BidCalls returns how many /bid requests the fake received.
func (f *FakeDSP) BidCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bidCalls
}

// BidRequests returns a copy of every decoded bid request the fake received,
// in arrival order — what an external buyer actually saw cross the wire.
func (f *FakeDSP) BidRequests() []openrtb.BidRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]openrtb.BidRequest, len(f.bidReqs))
	copy(out, f.bidReqs)
	return out
}

// WinCalls returns a copy of the recorded /win callbacks.
func (f *FakeDSP) WinCalls() []FakeNotify {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]FakeNotify, len(f.winCalls))
	copy(out, f.winCalls)
	return out
}

// LossCalls returns a copy of the recorded /loss callbacks.
func (f *FakeDSP) LossCalls() []FakeNotify {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]FakeNotify, len(f.lossCalls))
	copy(out, f.lossCalls)
	return out
}
