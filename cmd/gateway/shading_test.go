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
	// advStats is THIS advertiser's own per-placement win/loss. Asserts tenant
	// scope, and includes pl-3 — a placement the advertiser bid on but never
	// delivered an impression (lost), to exercise the union.
	advStats := func(_ context.Context, accountID string) (map[string]bidshading.PlacementStats, error) {
		if accountID != "adv-1" {
			t.Fatalf("advertiserStats got account %q, want adv-1 (tenant scope leaked)", accountID)
		}
		return map[string]bidshading.PlacementStats{
			"pl-1": {TotalBids: 8, Wins: 5, Losses: 3, WinRate: 0.625, AvgClearing: 2.1},
			"pl-2": {TotalBids: 4, Wins: 2, Losses: 2, WinRate: 0.5},
			"pl-3": {TotalBids: 6, Wins: 0, Losses: 6, WinRate: 0}, // bid, never delivered
		}, nil
	}
	deps := shadingDeps{placementSpend: spend, marketplaceStats: mkt, advertiserStats: advStats}

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
	// 2 delivered (pl-1, pl-2) + pl-3 unioned in from the advertiser's own tracker
	// stats (bid but never delivered). pl-cross (marketplace-only) must NOT appear.
	if len(resp.Placements) != 3 {
		t.Fatalf("placements=%d, want 3 (2 delivered + 1 bid-only union)", len(resp.Placements))
	}
	for _, p := range resp.Placements {
		if p.PlacementID == "pl-cross" {
			t.Fatalf("cross-tenant placement pl-cross leaked into advertiser view")
		}
	}
	// Money totals: 150 impressions, $350 spend (pl-3 delivered nothing).
	if resp.YourTotals.Impressions != 150 || resp.YourTotals.SpendUSD != 350 {
		t.Errorf("totals imp=%d spend=%.2f, want 150 / 350", resp.YourTotals.Impressions, resp.YourTotals.SpendUSD)
	}
	// Per-advertiser win/loss totals: wins 5+2+0=7, losses 3+2+6=11, bids 8+4+6=18.
	if resp.YourTotals.Wins != 7 || resp.YourTotals.Losses != 11 || resp.YourTotals.Bids != 18 {
		t.Errorf("win/loss totals = %+v, want wins 7 / losses 11 / bids 18", resp.YourTotals)
	}
	byID := map[string]shadingPlacementRow{}
	for _, p := range resp.Placements {
		byID[p.PlacementID] = p
	}
	// pl-1: own win/loss (genuinely per-advertiser) + marketplace context + own spend.
	if p := byID["pl-1"]; !p.HasYourStats || p.YourWins != 5 || p.YourLosses != 3 || p.YourWinRate != 0.625 ||
		!p.HasMarketplaceStats || p.MarketplaceWinRate != 0.4 || p.YourAvgClearingCPM != 2.00 {
		t.Errorf("pl-1 wrong: %+v", p)
	}
	if p := byID["pl-2"]; p.HasMarketplaceStats {
		t.Errorf("pl-2 should have NO marketplace stats (not in tracker): %+v", p)
	}
	// pl-3: bid-only union — own win/loss present, zero delivered impressions.
	if p := byID["pl-3"]; !p.HasYourStats || p.YourBids != 6 || p.YourImpressions != 0 {
		t.Errorf("pl-3 (bid-only union) wrong: %+v", p)
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
	}, advertiserStats: func(context.Context, string) (map[string]bidshading.PlacementStats, error) {
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
