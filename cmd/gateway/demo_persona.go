package main

// demo_persona.go — the shared fire/beacon machinery both guided demos that
// fire a REAL ad request use (the "Auction Trace" and "Billing / Money Flow"
// demos). Keeping this in one place means there is exactly one code path that:
//
//   - fires a fixed-persona display request at the REAL SSP serve endpoint
//     (routes.SSPServe), capturing the X-Trace-Id header + decoding the serve
//     JSON (winner campaign/advertiser, clearing-price CPM, the signed beacon
//     URLs); and
//   - GETs a server-returned, already-HMAC-signed tracking beacon the way a
//     browser render does (fireBeacon).
//
// The two demos differ in what they do AFTER the fire — the trace demo fires
// the beacon inline and polls the trace reader; the billing demo snapshots the
// advertiser's balance BEFORE firing the impression beacon so it can show the
// before→after drawdown. So this file exposes the low-level fire (serveOnce)
// and beacon (fireBeacon) as reusable pieces, and each demo keeps its own
// orchestrator/steps.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	neturl "net/url"
	"strings"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/simulator/request"
)

// personaServeResult is the full subset of the SSP serve JSON the demos need.
// The impression/viewability URLs are the REAL HMAC-signed beacons the ad
// server built for THIS render — firing them is exactly what a browser does on
// render, and (for the billing demo) firing the impression beacon is the event
// that actually bills. campaign_id / advertiser_id identify the real winner the
// spend is attributed to.
type personaServeResult struct {
	TraceID string
	// Served is true when an ad served (a real DSP won); false on a genuine
	// no-bid (nothing rendered, nothing billed).
	Served bool
	// ClearingPriceCPM is the winning bid's CPM (price per 1000 impressions).
	// The per-impression cost is this ÷ 1000.
	ClearingPriceCPM float64
	CampaignID       string
	AdvertiserID     string // = the advertiser's account_id (advertiser_balances.account_id)
	Currency         string
	ImpressionURL    string
	ViewabilityURL   string
}

// sspServeJSON is the raw serve response fields we decode. Mirrors the ad
// server's serveAdResponse (cmd/ssp) — only the fields the demos read.
type sspServeJSON struct {
	NoBid          bool    `json:"nobid"`
	ImpressionURL  string  `json:"impression_url"`
	ViewabilityURL string  `json:"viewability_url"`
	ClearingPrice  float64 `json:"clearing_price"`
	CampaignID     string  `json:"campaign_id"`
	AdvertiserID   string  `json:"advertiser_id"`
	Currency       string  `json:"currency"`
}

// serveOnce fires ONE display request for the named persona/placement at the
// real SSP serve endpoint and returns the decoded result WITHOUT firing any
// beacon — the caller decides when (the billing demo snapshots between the
// auction and the impression). The trace_id comes from the X-Trace-Id response
// header. An error is returned only when the request itself failed or no
// trace_id came back; a genuine no-bid is a successful call with Served=false.
func serveOnce(ctx context.Context, client *http.Client, sspURL, persona, placement string) (personaServeResult, error) {
	p, ok := request.PersonaByName(persona)
	if !ok {
		return personaServeResult{}, fmt.Errorf("unknown demo persona %q", persona)
	}
	params := p.QueryParams(placement, request.Display)
	url := strings.TrimRight(sspURL, "/") + routes.SSPServe + "?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return personaServeResult{}, err
	}
	// Browser-shaped so downstream fraud checks don't drop the request.
	req.Header.Set("User-Agent", "Mozilla/5.0 (adtech-demo)")
	req.Header.Set("Referer", "https://demo.adtech.local/")

	resp, err := client.Do(req)
	if err != nil {
		return personaServeResult{}, err
	}
	defer resp.Body.Close()

	out := personaServeResult{TraceID: resp.Header.Get(constants.HeaderTraceID)}
	if resp.StatusCode == http.StatusOK {
		var sr sspServeJSON
		if err := json.NewDecoder(resp.Body).Decode(&sr); err == nil {
			out.Served = !sr.NoBid
			out.ClearingPriceCPM = sr.ClearingPrice
			out.CampaignID = sr.CampaignID
			out.AdvertiserID = sr.AdvertiserID
			out.Currency = sr.Currency
			out.ImpressionURL = sr.ImpressionURL
			out.ViewabilityURL = sr.ViewabilityURL
			// The display serve path leaves the top-level campaign/advertiser
			// fields empty, but the impression beacon it built carries them as
			// query params (advid / cid / cur) — the same ids the tracker bills
			// against. Parse them from the beacon as the authoritative fallback.
			if out.AdvertiserID == "" || out.CampaignID == "" {
				if bu, perr := neturl.Parse(sr.ImpressionURL); perr == nil {
					q := bu.Query()
					if out.AdvertiserID == "" {
						out.AdvertiserID = q.Get("advid")
					}
					if out.CampaignID == "" {
						out.CampaignID = q.Get("cid")
					}
					if out.Currency == "" {
						out.Currency = q.Get("cur")
					}
				}
			}
		}
	}
	if out.TraceID == "" {
		return personaServeResult{}, fmt.Errorf("no %s header on serve response (status %d)", constants.HeaderTraceID, resp.StatusCode)
	}
	return out, nil
}

// fireBeacon GETs a server-returned tracking URL (already HMAC-signed) the way a
// browser render would. Best-effort: a beacon failure just means the event
// won't land. Returns whether it fired OK (status < 400).
func fireBeacon(ctx context.Context, client *http.Client, log *slog.Logger, beaconURL string) bool {
	beaconURL = strings.TrimSpace(beaconURL)
	if beaconURL == "" {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, beaconURL, nil)
	if err != nil {
		log.Warn("demo: bad beacon url", "error", err)
		return false
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (adtech-demo)")
	resp, err := client.Do(req)
	if err != nil {
		log.Warn("demo: beacon fire failed", "error", err)
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode < 400
}
