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
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
)

func TestShadingHandler(t *testing.T) {
	adv := &auth.Claims{UserID: "u1", AccountID: "adv-1", AccountType: auth.AccountAdvertiser, Permissions: []string{"reports:read"}}
	pub := &auth.Claims{UserID: "u2", AccountID: "pub-1", AccountType: auth.AccountPublisher, Permissions: []string{"reports:read"}}
	staff := &auth.Claims{UserID: "u3", AccountType: auth.AccountStaff, Permissions: []string{"*"}}

	// placementSpend asserts it was scoped to the caller's account, and returns
	// the advertiser's OWN per-placement spend. YourImpressions doubles as the
	// durable WIN count (a delivered impression = an auction this account won).
	spend := func(_ context.Context, accountID string, _ time.Time) ([]shadingPlacementRow, error) {
		if accountID != "adv-1" {
			t.Fatalf("placementSpend got account %q, want adv-1 (tenant scope leaked)", accountID)
		}
		return []shadingPlacementRow{
			{PlacementID: "pl-1", YourImpressions: 5, YourAvgClearingCPM: 2.00, YourSpendUSD: 200},
			{PlacementID: "pl-2", YourImpressions: 2, YourAvgClearingCPM: 3.00, YourSpendUSD: 150},
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
	// losses is THIS advertiser's own durable per-placement LOSS counts (from the
	// auction_losses CH table). Asserts tenant scope, and includes pl-3 — a
	// placement the advertiser bid on and LOST but never delivered an impression,
	// to exercise the union.
	losses := func(_ context.Context, accountID string, _ time.Time) (map[string]int64, error) {
		if accountID != "adv-1" {
			t.Fatalf("advertiserLosses got account %q, want adv-1 (tenant scope leaked)", accountID)
		}
		return map[string]int64{
			"pl-1": 3, // 5 wins + 3 losses = 8 bids, win rate 0.625
			"pl-2": 2, // 2 wins + 2 losses = 4 bids, win rate 0.5
			"pl-3": 6, // bid, never delivered: 0 wins + 6 losses
		}, nil
	}
	deps := shadingDeps{placementSpend: spend, advertiserLosses: losses, marketplaceStats: mkt}

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
	// 2 delivered (pl-1, pl-2) + pl-3 unioned in from the advertiser's own durable
	// losses (bid+lost, never delivered). pl-cross (marketplace-only) must NOT appear.
	if len(resp.Placements) != 3 {
		t.Fatalf("placements=%d, want 3 (2 delivered + 1 loss-only union)", len(resp.Placements))
	}
	for _, p := range resp.Placements {
		if p.PlacementID == "pl-cross" {
			t.Fatalf("cross-tenant placement pl-cross leaked into advertiser view")
		}
	}
	// Money totals: 7 impressions, $350 spend (pl-3 delivered nothing).
	if resp.YourTotals.Impressions != 7 || resp.YourTotals.SpendUSD != 350 {
		t.Errorf("totals imp=%d spend=%.2f, want 7 / 350", resp.YourTotals.Impressions, resp.YourTotals.SpendUSD)
	}
	// Durable win/loss totals: wins = impressions 5+2+0=7, losses 3+2+6=11, bids 7+11=18.
	if resp.YourTotals.Wins != 7 || resp.YourTotals.Losses != 11 || resp.YourTotals.Bids != 18 {
		t.Errorf("win/loss totals = %+v, want wins 7 / losses 11 / bids 18", resp.YourTotals)
	}
	byID := map[string]shadingPlacementRow{}
	for _, p := range resp.Placements {
		byID[p.PlacementID] = p
	}
	// pl-1: durable win/loss (wins=impressions, losses=auction_losses) + marketplace + own spend.
	if p := byID["pl-1"]; !p.HasYourStats || p.YourWins != 5 || p.YourLosses != 3 || p.YourBids != 8 || p.YourWinRate != 0.625 ||
		!p.HasMarketplaceStats || p.MarketplaceWinRate != 0.4 || p.YourAvgClearingCPM != 2.00 {
		t.Errorf("pl-1 wrong: %+v", p)
	}
	if p := byID["pl-2"]; p.HasMarketplaceStats {
		t.Errorf("pl-2 should have NO marketplace stats (not in tracker): %+v", p)
	}
	// pl-3: loss-only union — own losses present, zero delivered impressions/wins.
	if p := byID["pl-3"]; !p.HasYourStats || p.YourBids != 6 || p.YourLosses != 6 || p.YourWins != 0 || p.YourImpressions != 0 {
		t.Errorf("pl-3 (loss-only union) wrong: %+v", p)
	}

	// Publisher → 403 (advertiser view only).
	rec = httptest.NewRecorder()
	shadingHandler(deps, quietLog())(rec, withClaims(httptest.NewRequest(http.MethodGet, "/v1/api/shading", nil), pub))
	if rec.Code != http.StatusForbidden {
		t.Errorf("publisher GET code=%d, want 403", rec.Code)
	}

	// Staff with NO act-as → 403 (they have the dedicated staff view; no single
	// own-account here).
	rec = httptest.NewRecorder()
	shadingHandler(deps, quietLog())(rec, withClaims(httptest.NewRequest(http.MethodGet, "/v1/api/shading", nil), staff))
	if rec.Code != http.StatusForbidden {
		t.Errorf("staff GET code=%d, want 403", rec.Code)
	}

	// Staff IMPERSONATING an advertiser (act-as "advertiser:adv-1") → 200, scoped
	// to the impersonated account — the "viewing as" case from the staff portal.
	// (A staff session is type=staff; the old code 403'd it. CanAccessAccount lets
	// a platform user act as any account.)
	rec = httptest.NewRecorder()
	impReq := withClaims(httptest.NewRequest(http.MethodGet, "/v1/api/shading", nil), staff)
	impReq.Header.Set(constants.HeaderActAs, "advertiser:adv-1")
	shadingHandler(deps, quietLog())(rec, impReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("staff act-as advertiser GET code=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	var imp shadingResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &imp)
	if imp.AccountID != "adv-1" {
		t.Errorf("staff act-as: account_id=%q, want adv-1 (impersonated)", imp.AccountID)
	}

	// POST → 405 (read-only).
	rec = httptest.NewRecorder()
	shadingHandler(deps, quietLog())(rec, withClaims(httptest.NewRequest(http.MethodPost, "/v1/api/shading", nil), adv))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST code=%d, want 405", rec.Code)
	}

	// Marketplace + losses unavailable → still 200 with the advertiser's own money
	// numbers (spend/clearing don't depend on the DSP or auction_losses).
	degraded := shadingDeps{placementSpend: spend, marketplaceStats: func(context.Context) (map[string]bidshading.PlacementStats, error) {
		return nil, context.DeadlineExceeded
	}, advertiserLosses: func(context.Context, string, time.Time) (map[string]int64, error) {
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
