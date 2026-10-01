package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/bidshading"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
)

// shadingWindow is how far back we look for the advertiser's own delivered
// impressions when deriving which placements they actually compete on.
const shadingWindow = 30 * 24 * time.Hour

// shadingPlacementRow is one placement the advertiser actually bid on and won,
// carrying BOTH an honestly per-advertiser number and the DSP-wide marketplace
// context for the same placement — kept in separate, clearly named fields so a
// reader can never mistake one for the other.
type shadingPlacementRow struct {
	PlacementID string `json:"placement_id"`

	// --- The advertiser's OWN outcome (from THEIR account's impressions). ---
	// YourImpressions is the count of impressions THIS advertiser won & served on
	// this placement in the window. YourAvgClearingCPM is the average price THIS
	// advertiser actually paid per impression (clearing_price_usd) — the real,
	// per-advertiser number. YourSpendUSD is their total paid on this placement.
	YourImpressions    int64   `json:"your_impressions"`
	YourAvgClearingCPM float64 `json:"your_avg_clearing_cpm"`
	YourSpendUSD       float64 `json:"your_spend_usd"`

	// --- The advertiser's OWN win/loss (genuinely per-advertiser, DURABLE). ---
	// Sourced from ClickHouse — wins from this account's delivered impressions
	// (impressions.account_id), losses from the durable auction_losses table
	// (account_id from the DSP loss-notice). Both keyed on the RAW advertiser
	// account the session carries, so they reconcile on the same key. Unlike the
	// old in-memory DSP tracker, these SURVIVE a DSP redeploy. YourBids =
	// wins + losses; YourWinRate = wins / bids. HasYourStats says whether this
	// account had any recorded bid activity (win or loss) on the placement.
	HasYourStats bool    `json:"has_your_stats"`
	YourBids     int     `json:"your_bids"`
	YourWins     int     `json:"your_wins"`
	YourLosses   int     `json:"your_losses"`
	YourWinRate  float64 `json:"your_win_rate"`

	// --- DSP-WIDE marketplace context for this placement (NOT this advertiser). ---
	// These come from the DSP's per-placement shading tracker, which aggregates
	// across ALL advertisers our DSP bids for. Populated only when the tracker has
	// data for this placement; HasMarketplaceStats says whether it does.
	HasMarketplaceStats    bool    `json:"has_marketplace_stats"`
	MarketplaceBids        int     `json:"marketplace_bids"`
	MarketplaceWinRate     float64 `json:"marketplace_win_rate"`
	MarketplaceAvgClearing float64 `json:"marketplace_avg_clearing"`
	MarketplaceBelowFloor  int     `json:"marketplace_below_floor"`
	MarketplaceOutbid      int     `json:"marketplace_outbid"`
}

// shadingResponse is the advertiser-scoped transparency payload. The top-level
// labels + the per-field naming make the honesty constraint explicit: the
// "your_*" fields are genuinely this advertiser's own data; the "marketplace_*"
// fields are DSP-wide context for the inventory they bid on.
type shadingResponse struct {
	AccountID    string                `json:"account_id"`
	WindowDays   int                   `json:"window_days"`
	Placements   []shadingPlacementRow `json:"placements"`
	YourTotals   shadingTotals         `json:"your_totals"`
	Explanations shadingExplanations   `json:"explanations"`
}

type shadingTotals struct {
	Placements     int     `json:"placements"`
	Impressions    int64   `json:"impressions"`
	SpendUSD       float64 `json:"spend_usd"`
	AvgClearingCPM float64 `json:"avg_clearing_cpm"`
	Bids           int     `json:"bids"`
	Wins           int     `json:"wins"`
	Losses         int     `json:"losses"`
	WinRate        float64 `json:"win_rate"`
}

// shadingExplanations is the machine-readable copy that labels exactly what each
// family of numbers measures (the page also renders prose; keeping it here means
// the API self-documents the honesty boundary).
type shadingExplanations struct {
	WhatIsBidShading   string `json:"what_is_bid_shading"`
	YourNumbers        string `json:"your_numbers"`
	MarketplaceNumbers string `json:"marketplace_numbers"`
}

// shadingDeps are the collaborators the handler needs, injected for testability.
type shadingDeps struct {
	// placementSpend returns this advertiser's own per-placement impressions,
	// avg clearing CPM, and spend over the window (account-scoped — forced).
	// YourImpressions here is ALSO the durable WIN count per placement (a
	// delivered impression is an auction this account won and served).
	placementSpend func(ctx context.Context, accountID string, since time.Time) ([]shadingPlacementRow, error)
	// advertiserLosses returns THIS advertiser's OWN per-placement LOSS counts,
	// from the durable auction_losses ClickHouse table (account-scoped — forced).
	// Keyed on the same raw account as placementSpend, so wins + losses reconcile.
	// This is the phase-2 durability replacement for the DSP's in-memory tracker:
	// it survives a DSP redeploy.
	advertiserLosses func(ctx context.Context, accountID string, since time.Time) (map[string]int64, error)
	// marketplaceStats returns the DSP-wide per-placement shading tracker state
	// (live, in-memory — labelled as such; NOT this advertiser's own numbers).
	marketplaceStats func(ctx context.Context) (map[string]bidshading.PlacementStats, error)
}

// shadingHandler serves GET /v1/api/shading — the advertiser-facing bid-shading /
// win-rate transparency view. It is tenant-scoped to the caller's own account
// (never cross-tenant): the per-advertiser numbers come from a query FORCED to the
// session account_id, and the only DSP-wide data shown is the marketplace shading
// context for placements the advertiser themselves competed on. reports:read.
//
// HONESTY: the DSP shading tracker is per-placement and DSP-WIDE (aggregated across
// every advertiser we bid for), so it is NEVER presented as "your win rate". It is
// exposed under marketplace_* fields with an explicit label. The genuinely
// per-advertiser numbers (your_*) are derived from THIS account's own impressions.
func shadingHandler(deps shadingDeps, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}

		// Tenant scope + effective account. Same rule as report_jobs.go's
		// effectiveReportAccount so staff "viewing as" / agency act-as works:
		//   - act-as target present (staff impersonating, or agency on a managed
		//     account) + CanAccessAccount → scope to the IMPERSONATED account,
		//     regardless of the caller's own type (a staff session is type=staff).
		//   - otherwise require an advertiser/agency session and scope to its own
		//     account (a publisher, or staff with no act-as, is refused — staff
		//     have the dedicated /v1/api/staff/shading view).
		accountID := claims.AccountID
		if target := middleware.ActAsTarget(r); target != "" {
			_, id := middleware.ParseActAsTarget(target)
			if id == "" || !auth.CanAccessAccount(claims, id) {
				http.Error(w, `{"error":"forbidden: cannot act as that account"}`, http.StatusForbidden)
				return
			}
			accountID = id
		} else if claims.AccountType != auth.AccountAdvertiser && claims.AccountType != auth.AccountAgency {
			http.Error(w, `{"error":"forbidden: advertiser view only"}`, http.StatusForbidden)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()

		since := time.Now().Add(-shadingWindow)
		rows, err := deps.placementSpend(ctx, accountID, since)
		if err != nil {
			log.Error("shading: advertiser placement spend query failed", "account", accountID, "error", err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}

		// The advertiser's OWN per-placement LOSSES — durable, from ClickHouse
		// (auction_losses), account-forced. Non-fatal if the query fails: the
		// money numbers + wins (impressions) above still stand; losses just stay 0.
		// This is the phase-2 durability source — it survives a DSP redeploy,
		// unlike the old in-memory DSP tracker.
		losses, err := deps.advertiserLosses(ctx, accountID, since)
		if err != nil {
			log.Warn("shading: per-advertiser losses unavailable", "account", accountID, "error", err)
			losses = nil
		}
		// DSP-wide marketplace context (live, in-memory). Also non-fatal —
		// marketplace_* just stays unpopulated if the DSP can't be reached.
		mkt, err := deps.marketplaceStats(ctx)
		if err != nil {
			log.Warn("shading: marketplace stats unavailable; returning advertiser-own numbers only", "error", err)
			mkt = nil
		}

		// Union: the money rows are delivered impressions (wins). Add any placement
		// the advertiser LOST on but never delivered an impression, so the win/loss
		// picture is complete, not win-only.
		seen := make(map[string]bool, len(rows))
		for i := range rows {
			seen[rows[i].PlacementID] = true
		}
		for pid := range losses {
			if !seen[pid] {
				rows = append(rows, shadingPlacementRow{PlacementID: pid})
			}
		}

		var totals shadingTotals
		for i := range rows {
			// Wins = this account's delivered impressions on the placement (durable,
			// account-scoped). Losses = durable auction_losses count. Bids = the sum.
			wins := int(rows[i].YourImpressions)
			loss := int(losses[rows[i].PlacementID])
			if wins > 0 || loss > 0 {
				bids := wins + loss
				rows[i].HasYourStats = true
				rows[i].YourBids = bids
				rows[i].YourWins = wins
				rows[i].YourLosses = loss
				if bids > 0 {
					rows[i].YourWinRate = float64(wins) / float64(bids)
				}
				totals.Bids += bids
				totals.Wins += wins
				totals.Losses += loss
			}
			if s, ok := mkt[rows[i].PlacementID]; ok && s.TotalBids > 0 {
				rows[i].HasMarketplaceStats = true
				rows[i].MarketplaceBids = s.TotalBids
				rows[i].MarketplaceWinRate = s.WinRate
				rows[i].MarketplaceAvgClearing = s.AvgClearing
				rows[i].MarketplaceBelowFloor = s.BelowFloor
				rows[i].MarketplaceOutbid = s.Outbid
			}
			totals.Impressions += rows[i].YourImpressions
			totals.SpendUSD += rows[i].YourSpendUSD
		}
		totals.Placements = len(rows)
		if totals.Impressions > 0 {
			totals.AvgClearingCPM = totals.SpendUSD / float64(totals.Impressions)
		}
		if totals.Bids > 0 {
			totals.WinRate = float64(totals.Wins) / float64(totals.Bids)
		}

		// Stable order: biggest spend first (what the advertiser cares about).
		sort.SliceStable(rows, func(i, j int) bool {
			return rows[i].YourSpendUSD > rows[j].YourSpendUSD
		})

		resp := shadingResponse{
			AccountID:  accountID,
			WindowDays: int(shadingWindow / (24 * time.Hour)),
			Placements: rows,
			YourTotals: totals,
			Explanations: shadingExplanations{
				WhatIsBidShading: "In a first-price auction the winner pays exactly what they bid. " +
					"Bid shading lowers our DSP's bid toward the true clearing price so you win " +
					"without overpaying — it reduces what you pay, not whether the auction is first-price.",
				YourNumbers: "The 'your_*' figures are YOUR account's own, sourced durably from " +
					"ClickHouse: delivered impressions (your wins) and the price YOU actually paid " +
					"(clearing_price_usd) on each placement, plus your losses (auction_losses) and the " +
					"resulting win rate on that inventory. Genuinely per-advertiser, and they survive " +
					"a DSP restart (no longer read from the DSP's in-memory counters).",
				MarketplaceNumbers: "The 'marketplace_*' figures are our DSP's WHOLE-MARKETPLACE win/loss " +
					"behaviour on this placement, aggregated across every advertiser we bid for — NOT " +
					"your personal win rate. They show how competitive the inventory you bid on is.",
			},
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// reportingPlacementSpend builds the placementSpend dependency: it POSTs an
// account-FORCED query to the reporting service for the advertiser's own
// per-placement impressions, spend, and avg clearing CPM. The account_id filter
// is set server-side here (not from the client body), so it cannot be widened.
func reportingPlacementSpend(reportingURL string, log *slog.Logger) func(context.Context, string, time.Time) ([]shadingPlacementRow, error) {
	return func(ctx context.Context, accountID string, since time.Time) ([]shadingPlacementRow, error) {
		params := analytics.QueryParams{
			Table:      "impressions",
			Metrics:    []string{"count", "sum_cost", "avg_cost"},
			Dimensions: []string{"placement_id"},
			Filters:    map[string]string{"account_id": accountID},
			TimeFrom:   since,
			TimeTo:     time.Now(),
			OrderBy:    "sum_cost",
			OrderDir:   "desc",
			Limit:      200,
		}
		body, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, reportingURL+routes.ReportingQuery, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("reporting query returned %d", resp.StatusCode)
		}
		var qr analytics.QueryResult
		if err := json.NewDecoder(resp.Body).Decode(&qr); err != nil {
			return nil, err
		}
		return parsePlacementSpend(qr), nil
	}
}

// parsePlacementSpend maps the reporting QueryResult (columns placement_id, count,
// sum_cost, avg_cost) into per-placement rows. Columns are matched by name so a
// column-order change upstream doesn't silently misalign the money.
func parsePlacementSpend(qr analytics.QueryResult) []shadingPlacementRow {
	idx := map[string]int{}
	for i, c := range qr.Columns {
		idx[c] = i
	}
	pi, ci, si, ai := idx["placement_id"], idx["count"], idx["sum_cost"], idx["avg_cost"]
	var out []shadingPlacementRow
	for _, row := range qr.Rows {
		var r shadingPlacementRow
		if pi >= 0 && pi < len(row) {
			r.PlacementID = toStr(row[pi])
		}
		if r.PlacementID == "" {
			continue
		}
		if ci >= 0 && ci < len(row) {
			r.YourImpressions = toInt64(row[ci])
		}
		if si >= 0 && si < len(row) {
			r.YourSpendUSD = toFloat(row[si])
		}
		if ai >= 0 && ai < len(row) {
			r.YourAvgClearingCPM = toFloat(row[ai])
		}
		out = append(out, r)
	}
	return out
}

// dspMarketplaceStats builds the marketplaceStats dependency: it GETs the DSP's
// per-placement shading map (DSP-wide, across all advertisers).
func dspMarketplaceStats(dspURL string) func(context.Context) (map[string]bidshading.PlacementStats, error) {
	return func(ctx context.Context) (map[string]bidshading.PlacementStats, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, dspURL+routes.DSPShading, nil)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("dsp shading returned %d", resp.StatusCode)
		}
		var m map[string]bidshading.PlacementStats
		if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
			return nil, err
		}
		return m, nil
	}
}

// reportingAdvertiserLosses builds the advertiserLosses dependency: it POSTs an
// account-FORCED query to the reporting service for THIS advertiser's own durable
// per-placement loss counts from the auction_losses ClickHouse table. The
// account_id filter is set server-side (not from the client body), so it cannot
// be widened. This is the phase-2 durability replacement for the DSP's in-memory
// per-advertiser tally — the loss counts survive a DSP redeploy.
func reportingAdvertiserLosses(reportingURL string, log *slog.Logger) func(context.Context, string, time.Time) (map[string]int64, error) {
	return func(ctx context.Context, accountID string, since time.Time) (map[string]int64, error) {
		params := analytics.QueryParams{
			Table:      "auction_losses",
			Metrics:    []string{"count"},
			Dimensions: []string{"placement_id"},
			Filters:    map[string]string{"account_id": accountID},
			TimeFrom:   since,
			TimeTo:     time.Now(),
			Limit:      500,
		}
		body, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, reportingURL+routes.ReportingQuery, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("reporting auction_losses query returned %d", resp.StatusCode)
		}
		var qr analytics.QueryResult
		if err := json.NewDecoder(resp.Body).Decode(&qr); err != nil {
			return nil, err
		}
		return parseLossCounts(qr), nil
	}
}

// parseLossCounts maps the reporting QueryResult (columns placement_id, count)
// into a per-placement loss count map. Columns matched by name so a column-order
// change upstream doesn't misalign.
func parseLossCounts(qr analytics.QueryResult) map[string]int64 {
	idx := map[string]int{}
	for i, c := range qr.Columns {
		idx[c] = i
	}
	pi, ci := idx["placement_id"], idx["count"]
	out := map[string]int64{}
	for _, row := range qr.Rows {
		if pi < 0 || pi >= len(row) {
			continue
		}
		pid := toStr(row[pi])
		if pid == "" {
			continue
		}
		var n int64
		if ci >= 0 && ci < len(row) {
			n = toInt64(row[ci])
		}
		out[pid] = n
	}
	return out
}

// --- JSON scalar coercion (the reporting result rows are []interface{}). ---

func toStr(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%v", v)
}
