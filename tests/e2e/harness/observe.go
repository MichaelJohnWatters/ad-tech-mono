//go:build e2e

package harness

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// DSPCampaignIDs returns the campaign IDs currently in a DSP pod's warm
// cache, parsed from /v1/dsp/campaigns. Used to assert that a campaign
// created via direct SQL has propagated through the cache poll or NATS
// invalidate.
func (h *Harness) DSPCampaignIDs(t *testing.T, dspURL string) []string {
	t.Helper()
	rows := h.dspCampaignRows(t, dspURL)
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}

// DSPCampaignBudget returns the DailyBudget field for a specific campaign as
// the DSP pod currently has it in its warm cache. Returns (budget, true) when
// found, (0, false) when the campaign is absent. Used by invalidate/refresh
// tests to assert the cache picked up a Postgres budget change — presence
// alone isn't enough since the campaign may already be cached from earlier.
func (h *Harness) DSPCampaignBudget(t *testing.T, dspURL, campaignID string) (float64, bool) {
	t.Helper()
	for _, r := range h.dspCampaignRows(t, dspURL) {
		if r.ID == campaignID {
			return r.DailyBudget, true
		}
	}
	return 0, false
}

type dspCampaignRow struct {
	ID          string  `json:"ID"`
	DailyBudget float64 `json:"DailyBudget"`
}

func (h *Harness) dspCampaignRows(t *testing.T, dspURL string) []dspCampaignRow {
	t.Helper()
	body := h.getJSON(t, dspURL+"/v1/dsp/campaigns")
	var rows []dspCampaignRow
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatalf("decode dsp campaigns: %v", err)
	}
	return rows
}

// ExchangeDealIDs returns the deal IDs currently in the exchange warm cache.
func (h *Harness) ExchangeDealIDs(t *testing.T) []string {
	t.Helper()
	body := h.getJSON(t, h.URLs.Exchange+"/v1/openrtb/deals")
	var rows []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatalf("decode exchange deals: %v", err)
	}
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}

// SSPPlacementIDs returns placement IDs currently in the SSP warm cache.
func (h *Harness) SSPPlacementIDs(t *testing.T) []string {
	t.Helper()
	body := h.getJSON(t, h.URLs.SSP+"/v1/ssp/placements")
	var rows []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatalf("decode ssp placements: %v", err)
	}
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}

// AuctionWinCount queries the reporting service's in-memory analytics store
// for how many auction-win records exist for the given trace_id. Used by
// auction.win tests to verify the NATS event reached reporting exactly once
// (not zero, not double).
func (h *Harness) AuctionWinCount(t *testing.T, traceID string) int {
	t.Helper()
	body := h.getJSON(t, h.URLs.Reporting+routes.DebugAuctionWins+"?trace_id="+traceID)
	var out struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode auction win count: %v\nbody: %s", err, string(body))
	}
	return out.Count
}

// BillingSummary returns the reporting-side billing snapshot. Used to
// assert AuctionWinEvent → spend ledger.
func (h *Harness) BillingSummary(t *testing.T) map[string]any {
	t.Helper()
	body := h.getJSON(t, h.URLs.Reporting+"/v1/billing/summary")
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode billing summary: %v", err)
	}
	return out
}

func (h *Harness) getJSON(t *testing.T, url string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	resp, err := h.HTTP.Do(req)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get %s status %d: %s", url, resp.StatusCode, string(body))
	}
	return body
}
