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
	body := h.getJSON(t, h.URLs.Exchange+routes.DebugExchangeDeals)
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

// AuctionWinByBidModel filters AuctionWinCount by bid_model. Used by the
// new-event-types e2e tests to assert that DirectWin and PrebidOutboundWin
// events landed in the analytics store with the right BidModel tag (e.g.
// "direct:sponsorship" or "prebid_outbound").
func (h *Harness) AuctionWinByBidModel(t *testing.T, traceID, bidModel string) int {
	t.Helper()
	body := h.getJSON(t, h.URLs.Reporting+routes.DebugAuctionWins+"?trace_id="+traceID+"&bid_model="+bidModel)
	var out struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode auction win count: %v\nbody: %s", err, string(body))
	}
	return out.Count
}

// FreqCapBlock mirrors analytics.FreqCapBlock — one record per
// (user, campaign) suppression at the ad server.
type FreqCapBlock struct {
	TraceID     string    `json:"TraceID"`
	UserID      string    `json:"UserID"`
	CampaignID  string    `json:"CampaignID"`
	PlacementID string    `json:"PlacementID"`
	PublisherID string    `json:"PublisherID"`
	Timestamp   time.Time `json:"Timestamp"`
}

// FreqCapBlocksByCampaign returns the recorded suppression records
// for a campaign. Used by e2e to verify
// adtech.adserver.freq_cap_blocked propagated end-to-end.
func (h *Harness) FreqCapBlocksByCampaign(t *testing.T, campaignID string) []FreqCapBlock {
	t.Helper()
	body := h.getJSON(t, h.URLs.Reporting+routes.DebugFreqCapBlocks+"?campaign_id="+campaignID)
	var out []FreqCapBlock
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode freq cap blocks: %v\nbody: %s", err, string(body))
	}
	return out
}

// RenderFailure mirrors analytics.RenderFailure — one record per
// ad-server fallback (unknown creative, render error). Decoded from
// JSON over the debug endpoint.
type RenderFailure struct {
	TraceID     string    `json:"TraceID"`
	CampaignID  string    `json:"CampaignID"`
	CreativeID  string    `json:"CreativeID"`
	PlacementID string    `json:"PlacementID"`
	PublisherID string    `json:"PublisherID"`
	Reason      string    `json:"Reason"`
	Detail      string    `json:"Detail"`
	Timestamp   time.Time `json:"Timestamp"`
}

// RenderFailuresByCreative returns the recorded ad-server fallback
// records for a creative_id. Used by e2e to verify
// adtech.adserver.render_failed events flowed end-to-end.
func (h *Harness) RenderFailuresByCreative(t *testing.T, creativeID string) []RenderFailure {
	t.Helper()
	body := h.getJSON(t, h.URLs.Reporting+routes.DebugRenderFailures+"?creative_id="+creativeID)
	var out []RenderFailure
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode render failures: %v\nbody: %s", err, string(body))
	}
	return out
}

// TrackerRejection mirrors analytics.TrackerRejection — one record per
// pixel dropped at the tracker gate (invalid sig in strict mode, fraud
// blocked, dedup hit). Decoded from JSON over the debug endpoint.
type TrackerRejection struct {
	TraceID   string    `json:"TraceID"`
	EventType string    `json:"EventType"`
	Reason    string    `json:"Reason"`
	Detail    string    `json:"Detail"`
	Timestamp time.Time `json:"Timestamp"`
}

// TrackerRejectionsByTrace returns the recorded rejections for a
// trace_id, optionally filtered by reason. Empty reason = all reasons.
// Used by e2e to assert adtech.tracker.rejected events flowed for a
// specific rejection class.
func (h *Harness) TrackerRejectionsByTrace(t *testing.T, traceID, reason string) []TrackerRejection {
	t.Helper()
	q := "?trace_id=" + traceID
	if reason != "" {
		q += "&reason=" + reason
	}
	body := h.getJSON(t, h.URLs.Reporting+routes.DebugTrackerRejections+q)
	var out []TrackerRejection
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode tracker rejections: %v\nbody: %s", err, string(body))
	}
	return out
}

// TrackerRejectionsByReason returns the total rejection count for a
// given reason ("invalid_signature" / "fraud" / "dedup"). Used by
// fraud-volume dashboards + tests that want to assert "at least one
// of this reason landed since N".
func (h *Harness) TrackerRejectionsByReason(t *testing.T, reason string) int {
	t.Helper()
	body := h.getJSON(t, h.URLs.Reporting+routes.DebugTrackerRejections+"?reason="+reason)
	var out struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode rejection count: %v\nbody: %s", err, string(body))
	}
	return out.Count
}

// CampaignStateChange mirrors analytics.CampaignStateChange — the
// per-event record reporting captured for a pause / resume / archive.
// Fields decoded from JSON over the debug endpoint, so they're
// untyped strings (the analytics layer keeps everything as text for
// dashboard simplicity).
type CampaignStateChange struct {
	CampaignID string    `json:"CampaignID"`
	AccountID  string    `json:"AccountID"`
	OldState   string    `json:"OldState"`
	NewState   string    `json:"NewState"`
	Reason     string    `json:"Reason"`
	Timestamp  time.Time `json:"Timestamp"`
}

// CampaignStateChangesByCampaign returns the recorded state transitions
// reporting has seen for a campaign, in event-arrival order. Used to
// verify adtech.campaign.state_changed actually propagates through the
// DSP-mgmt → NATS → reporting pathway.
func (h *Harness) CampaignStateChangesByCampaign(t *testing.T, campaignID string) []CampaignStateChange {
	t.Helper()
	body := h.getJSON(t, h.URLs.Reporting+routes.DebugCampaignStateChanges+"?campaign_id="+campaignID)
	var out []CampaignStateChange
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode campaign state changes: %v\nbody: %s", err, string(body))
	}
	return out
}

// BudgetDepletionsByCampaign counts BudgetDepletedEvents reporting has
// seen for a campaign. Used to verify the DSP → NATS → reporting flow.
func (h *Harness) BudgetDepletionsByCampaign(t *testing.T, campaignID string) int {
	t.Helper()
	body := h.getJSON(t, h.URLs.Reporting+routes.DebugBudgetDepletions+"?campaign_id="+campaignID)
	var out struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode budget depletion count: %v\nbody: %s", err, string(body))
	}
	return out.Count
}

// ServeNoFillsByTrace counts ServeNoFill records for a trace_id.
func (h *Harness) ServeNoFillsByTrace(t *testing.T, traceID string) int {
	t.Helper()
	body := h.getJSON(t, h.URLs.Reporting+routes.DebugServeNoFills+"?trace_id="+traceID)
	var out struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode serve nofill count: %v\nbody: %s", err, string(body))
	}
	return out.Count
}

// MediaEventsByTrace counts video/audio engagement records, optionally
// filtered by channel ("video"/"audio"), event_type and error_code (numeric
// string, e.g. "405"; "" = no filter).
func (h *Harness) MediaEventsByTrace(t *testing.T, traceID, channel, eventType, errorCode string) int {
	t.Helper()
	q := "?trace_id=" + traceID
	if channel != "" {
		q += "&channel=" + channel
	}
	if eventType != "" {
		q += "&event_type=" + eventType
	}
	if errorCode != "" {
		q += "&error_code=" + errorCode
	}
	body := h.getJSON(t, h.URLs.Reporting+routes.DebugMediaEvents+q)
	var out struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode media event count: %v\nbody: %s", err, string(body))
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
	// Always attach the dev API key — debug endpoints ignore it, management
	// endpoints (DSP /v1/dsp/campaigns, SSP /v1/ssp/placements) now require
	// it after the Phase 4 auth wiring. Saves every observe helper from
	// having to know which endpoints are auth-wrapped.
	req.Header.Set("X-API-Key", DevAPIKey)
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
