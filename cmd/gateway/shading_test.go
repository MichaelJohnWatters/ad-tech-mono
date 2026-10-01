package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/bidshading"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
)

func TestShadingHandler(t *testing.T) {
	adv := &auth.Claims{UserID: "u1", AccountID: "adv-1", AccountType: auth.AccountAdvertiser, Permissions: []string{"reports:read"}}
	pub := &auth.Claims{UserID: "u2", AccountID: "pub-1", AccountType: auth.AccountPublisher, Permissions: []string{"reports:read"}}
	staff := &auth.Claims{UserID: "u3", AccountType: auth.AccountStaff, Permissions: []string{"*"}}

	// placementSpend asserts it was scoped to the caller's account, and returns
	// the advertiser's OWN per-placement spend.
	spend := func(_ context.Context, accountID string, _ time.Time) ([]shadingPlacementRow, error) {
		if accountID != "adv-1" {
			t.Fatalf("placementSpend got account %q, want adv-1 (tenant scope leaked)", accountID)
		}
		return []shadingPlacementRow{
			{PlacementID: "pl-1", YourImpressions: 100, YourAvgClearingCPM: 2.00, YourSpendUSD: 200},
			{PlacementID: "pl-2", YourImpressions: 50, YourAvgClearingCPM: 3.00, YourSpendUSD: 150},
		}, nil
	}
	mkt := func(_ context.Context) (map[string]bidshading.PlacementStats, error) {
		return map[string]bidshading.PlacementStats{
			"pl-1": {TotalBids: 10, Wins: 4, WinRate: 0.4, AvgClearing: 2.35, BelowFloor: 1, Outbid: 5},
			// pl-2 deliberately absent → marketplace fields stay unpopulated.
			// pl-cross belongs to another advertiser's inventory and must NOT appear
			// in the output because the advertiser never bid on it.
			"pl-cross": {TotalBids: 99, Wins: 90, WinRate: 0.9},
		}, nil
	}
	deps := shadingDeps{placementSpend: spend, marketplaceStats: mkt}

	// Advertiser GET → 200, their own numbers + marketplace context only for
	// placements they competed on.
	rec := httptest.NewRecorder()
	shadingHandler(deps, quietLog())(rec, withClaims(httptest.NewRequest(http.MethodGet, "/v1/api/shading", nil), adv))
	if rec.Code != http.StatusOK {
		t.Fatalf("advertiser GET code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp shadingResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.AccountID != "adv-1" {
		t.Errorf("account_id=%q, want adv-1", resp.AccountID)
	}
	if len(resp.Placements) != 2 {
		t.Fatalf("placements=%d, want 2 (only inventory the advertiser bid on)", len(resp.Placements))
	}
	for _, p := range resp.Placements {
		if p.PlacementID == "pl-cross" {
			t.Fatalf("cross-tenant placement pl-cross leaked into advertiser view")
		}
	}
	// Totals: 150 impressions, $350 spend, avg $350/150.
	if resp.YourTotals.Impressions != 150 || resp.YourTotals.SpendUSD != 350 {
		t.Errorf("totals imp=%d spend=%.2f, want 150 / 350", resp.YourTotals.Impressions, resp.YourTotals.SpendUSD)
	}
	// pl-1 sorts first (bigger spend), has marketplace stats; pl-2 has none.
	byID := map[string]shadingPlacementRow{}
	for _, p := range resp.Placements {
		byID[p.PlacementID] = p
	}
	if p := byID["pl-1"]; !p.HasMarketplaceStats || p.MarketplaceWinRate != 0.4 || p.YourAvgClearingCPM != 2.00 {
		t.Errorf("pl-1 wrong: %+v", p)
	}
	if p := byID["pl-2"]; p.HasMarketplaceStats {
		t.Errorf("pl-2 should have NO marketplace stats (not in tracker): %+v", p)
	}

	// Publisher → 403 (advertiser view only).
	rec = httptest.NewRecorder()
	shadingHandler(deps, quietLog())(rec, withClaims(httptest.NewRequest(http.MethodGet, "/v1/api/shading", nil), pub))
	if rec.Code != http.StatusForbidden {
		t.Errorf("publisher GET code=%d, want 403", rec.Code)
	}

	// Staff → 403 (they have the dedicated staff view; no single own-account here).
	rec = httptest.NewRecorder()
	shadingHandler(deps, quietLog())(rec, withClaims(httptest.NewRequest(http.MethodGet, "/v1/api/shading", nil), staff))
	if rec.Code != http.StatusForbidden {
		t.Errorf("staff GET code=%d, want 403", rec.Code)
	}

	// POST → 405 (read-only).
	rec = httptest.NewRecorder()
	shadingHandler(deps, quietLog())(rec, withClaims(httptest.NewRequest(http.MethodPost, "/v1/api/shading", nil), adv))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST code=%d, want 405", rec.Code)
	}

	// Marketplace unavailable → still 200 with the advertiser's own numbers
	// (the per-advertiser part doesn't depend on the DSP).
	degraded := shadingDeps{placementSpend: spend, marketplaceStats: func(context.Context) (map[string]bidshading.PlacementStats, error) {
		return nil, context.DeadlineExceeded
	}}
	rec = httptest.NewRecorder()
	shadingHandler(degraded, quietLog())(rec, withClaims(httptest.NewRequest(http.MethodGet, "/v1/api/shading", nil), adv))
	if rec.Code != http.StatusOK {
		t.Fatalf("degraded GET code=%d, want 200", rec.Code)
	}
	var d shadingResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &d)
	if len(d.Placements) != 2 || d.Placements[0].HasMarketplaceStats {
		t.Errorf("degraded should keep own numbers, drop marketplace: %+v", d.Placements)
	}
}

func TestParsePlacementSpend(t *testing.T) {
	qr := analytics.QueryResult{
		Columns: []string{"placement_id", "count", "sum_cost", "avg_cost"},
		Rows: [][]interface{}{
			{"pl-1", float64(100), float64(200), float64(2.0)},
			{"", float64(5), float64(5), float64(1)}, // blank placement id skipped
		},
	}
	got := parsePlacementSpend(qr)
	if len(got) != 1 {
		t.Fatalf("rows=%d, want 1 (blank id dropped)", len(got))
	}
	if got[0].PlacementID != "pl-1" || got[0].YourImpressions != 100 || got[0].YourSpendUSD != 200 || got[0].YourAvgClearingCPM != 2.0 {
		t.Errorf("parsed wrong: %+v", got[0])
	}
}
