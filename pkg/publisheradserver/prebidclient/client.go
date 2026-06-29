// Package prebidclient is the outbound side of our Prebid integration:
// publisher-adserver fans out to external Prebid Server endpoints when
// falling through to programmatic, so our publishers get demand from both
// our own SSP AND from any Prebid Server they've onboarded with.
//
// Inbound (someone else's Prebid calling us) lives in pkg/prebid +
// cmd/exchange/prebid.go. Outbound (us calling someone else's Prebid)
// is here.
package prebidclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tracing"
)

// Result is one Prebid Server's response, decoded into the fields the
// publisher-adserver uses to compare against other demand sources.
//
// HTML is the bid's adm field — the markup the Prebid Server-side bidder
// returned. Pubad renders it directly when this bid wins (no macro
// substitution, no ad-server hop, because we don't own the creative).
type Result struct {
	Endpoint      string
	NoBid         bool
	Seat          string
	Price         float64
	HTML          string
	CreativeID    string
	CampaignID    string
	DealID        string
	Currency      string
	Width         int
	Height        int
	LatencyMillis int64
	Err           error
}

// Client is a thin HTTP client for outbound Prebid auction calls.
type Client struct {
	http *http.Client
	log  *slog.Logger
}

// New constructs a Client. Timeout applies per outbound call; the parent
// context further bounds total fan-out time.
func New(timeout time.Duration, log *slog.Logger) *Client {
	return &Client{
		http: &http.Client{Timeout: timeout},
		log:  log,
	}
}

// FanOut calls every endpoint in parallel with the same BidRequest and
// returns all results (including no-bids and errors). Caller does the
// price comparison + winner selection.
//
// Empty `endpoints` returns nil — caller can use a `len(results)==0` check
// to skip the comparison fast-path when no Prebid Servers are configured.
func (c *Client) FanOut(ctx context.Context, endpoints []string, req openrtb.BidRequest) []Result {
	if len(endpoints) == 0 {
		return nil
	}

	results := make([]Result, len(endpoints))
	var wg sync.WaitGroup
	wg.Add(len(endpoints))
	for i, ep := range endpoints {
		i, ep := i, ep
		go func() {
			defer wg.Done()
			results[i] = c.callOne(ctx, ep, req)
		}()
	}
	wg.Wait()
	return results
}

func (c *Client) callOne(ctx context.Context, endpoint string, req openrtb.BidRequest) Result {
	start := time.Now()
	r := Result{Endpoint: endpoint}

	body, err := json.Marshal(req)
	if err != nil {
		r.Err = fmt.Errorf("marshal: %w", err)
		return r
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		r.Err = fmt.Errorf("build request: %w", err)
		return r
	}
	httpReq.Header.Set("Content-Type", "application/json")
	tracing.InjectHTTP(ctx, httpReq)

	resp, err := c.http.Do(httpReq)
	r.LatencyMillis = time.Since(start).Milliseconds()
	if err != nil {
		r.Err = fmt.Errorf("call: %w", err)
		return r
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		r.Err = fmt.Errorf("status %d", resp.StatusCode)
		return r
	}

	var bidResp openrtb.BidResponse
	if err := json.Unmarshal(respBody, &bidResp); err != nil {
		r.Err = fmt.Errorf("decode: %w", err)
		return r
	}
	if bidResp.NoBid || len(bidResp.SeatBid) == 0 || len(bidResp.SeatBid[0].Bid) == 0 {
		r.NoBid = true
		return r
	}

	sb := bidResp.SeatBid[0]
	b := sb.Bid[0]
	r.Seat = sb.Seat
	r.Price = b.Price
	r.HTML = b.AdM
	r.CreativeID = b.CrID
	r.CampaignID = b.CID
	r.DealID = b.DealID
	r.Currency = bidResp.Cur
	r.Width = b.W
	r.Height = b.H
	return r
}

// Highest picks the highest-priced non-no-bid, non-error result. Returns
// the zero Result + false if no eligible result exists. Used by pubad to
// pick the winning Prebid bid before comparing against the SSP's outcome.
func Highest(results []Result) (Result, bool) {
	best := Result{}
	any := false
	for _, r := range results {
		if r.Err != nil || r.NoBid {
			continue
		}
		if !any || r.Price > best.Price {
			best = r
			any = true
		}
	}
	return best, any
}
