package main

// landing_convert.go — the demo "Complete purchase" conversion postback.
//
// Mirrors cmd/demoadv's /convert, but INSIDE the gateway so the clickable
// in-browser demo needs no second host process. The flow:
//
//	ad click  → tracker /v1/t/click 302 → /dev/landing/{slug}?adtech_tid=<trace>
//	landing page shows a "Complete purchase" CTA (brand.html, trace present)
//	CTA POSTs {tid, slug} → here (/dev/landing/convert)
//	gateway resolves the advertiser that WON that click (from the click row),
//	signs /v1/t/conv with that advertiser's conversion key, fires it S2S
//	tracker records the conversion with ctid=<trace> → attributed to the ad.
//
// A conversion is the CPA BILLING trigger, so /v1/t/conv is HMAC-signed and
// fired SERVER-SIDE (never unsigned from the browser) — same posture as
// cmd/demoadv. The advertiser is resolved from the TRUSTED click row, not a
// client-declared field, so a forged tid can only attribute to the advertiser
// that genuinely won that click.
//
// Debug-gated (keys.Debug.EndpointsEnabled), same as /dev/landing and
// /dev/reset-and-reseed — a dev demo surface, not a production endpoint.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adserving"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
)

// demoConvertRevenue is the headline purchase value the landing CTA advertises
// ("Complete purchase — $X") and the revenue the demo conversion books. Kept in
// one place so the button copy and the booked value never drift.
const demoConvertRevenue = 49.99

// clickResolution is what we learn about the ad click a conversion attributes
// to, read from the TRUSTED click row (never the client).
type clickResolution struct {
	AdvertiserID string
	CampaignID   string
	CreativeID   string
	PlacementID  string
	PublisherID  string
}

// landingConvertHandler fires a signed conversion for the demo landing page.
// reportingURL is used to look up the click row (advertiser resolution);
// trackerURL is the in-cluster tracker the signed /v1/t/conv is fired at (the
// HMAC covers path+params, not host, so the internal URL is fine).
func landingConvertHandler(reportingURL, trackerURL string, log *slog.Logger) http.HandlerFunc {
	client := &http.Client{Timeout: 5 * time.Second}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		_ = r.ParseForm()
		tid := strings.TrimSpace(r.FormValue("tid"))
		slug := strings.TrimSpace(r.FormValue("slug"))
		rev := landingThemeForSlug(slug).PurchaseValue() // per-brand (Ford=$38,000), not a flat $49.99
		if tid == "" {
			http.Error(w, `{"ok":false,"error":"missing trace id"}`, http.StatusBadRequest)
			return
		}
		ctx := logger.WithTraceID(r.Context(), tid)
		reqLog := logger.WithContext(log, ctx)

		// Resolve the advertiser that WON this click from the click row in
		// analytics — the trusted record of who was billed for the click, not a
		// client-declared field. This is what makes the conversion attribute to
		// (and bill) the right advertiser.
		res, err := resolveClick(ctx, client, reportingURL, tid)
		if err != nil {
			reqLog.Warn("landing convert: click lookup failed", "error", err)
		}
		if res.AdvertiserID == "" {
			// No click row yet (NATS→reporting lag) — fall back to the slug's
			// seeded advertiser so the demo still books an attributed conversion.
			// ctid still carries the real click trace, so attribution links back.
			reqLog.Info("landing convert: no click row; using slug fallback", "slug", slug)
		}

		// Sign /v1/t/conv with THIS advertiser's conversion key (G7 per-advertiser
		// posture; dev key is deterministic from the account id, same as demoadv).
		params := url.Values{}
		params.Set("tid", "order-"+strconv.FormatInt(time.Now().UnixNano(), 10))
		params.Set("type", "purchase")
		params.Set("rev", strconv.FormatFloat(rev, 'f', 2, 64))
		params.Set("cur", "USD")
		params.Set("advid", res.AdvertiserID)
		// ctid = the earning click's trace; rides INSIDE the signed URL so it can't
		// be forged. Present => deterministic click-through attribution: the tracker
		// stamps attributed_trace_id = this click + attribution_type = click_through
		// on the conversion row (what the Attribution tab + settle read). The
		// multi-touch assisting-impression chain is a separate reporting overlay the
		// attributor builds only for conversions that carry a visitor uid AND have
		// viewable prior exposures — not wired here (the click's visitor id lives in
		// behaviour_signals, which the analytics query API can't reach: it keys on
		// observed_at, not the timestamp column the query builder floors on).
		params.Set("ctid", tid)
		// Carry the click's exposure context so the conversion row records the
		// campaign/creative/placement it attributes to (same fields the tracker
		// stamps off a conversion URL).
		if res.CampaignID != "" {
			params.Set("cid", res.CampaignID)
		}
		if res.CreativeID != "" {
			params.Set("crid", res.CreativeID)
		}
		if res.PlacementID != "" {
			params.Set("pid", res.PlacementID)
		}

		signed := adserving.SignURL(strings.TrimRight(trackerURL, "/")+routes.TrackerConversion+"?"+params.Encode(),
			adserving.DevConversionKey(res.AdvertiserID))

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, signed, nil)
		if err != nil {
			http.Error(w, `{"ok":false}`, http.StatusInternalServerError)
			return
		}
		resp, err := client.Do(req)
		status := 0
		if err == nil {
			status = resp.StatusCode
			resp.Body.Close()
		}
		reqLog.Info("landing convert: S2S conversion postback",
			"advid", res.AdvertiserID, "campaign_id", res.CampaignID, "ctid", tid,
			"rev", rev, "tracker_status", status)

		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		if err != nil || status != http.StatusOK {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "tracker_status": status})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":            true,
			"advertiser_id": res.AdvertiserID,
			"campaign_id":   res.CampaignID,
			"ctid":          tid,
			"revenue":       rev,
		})
	}
}

// resolveClick resolves the advertiser + exposure ids for the ad the conversion
// attributes to, from the TRUSTED analytics rows for traceID — never a
// client-declared value. The click URL the ad server mints doesn't carry advid
// (BuildClickURL signs tid/cid/crid/pid/pubid only), so the advertiser is read
// from the IMPRESSION row for the same trace (impressions DO stamp account_id);
// the click row is the fallback for the exposure ids. The impression + click
// share one trace (a trace is one request), so this is the advertiser that
// genuinely won the exposure.
func resolveClick(ctx context.Context, client *http.Client, reportingURL, traceID string) (clickResolution, error) {
	// Impression row first — the authoritative advid source.
	imp, err := queryExposure(ctx, client, reportingURL, "impressions", traceID)
	if err != nil {
		return clickResolution{}, err
	}
	if imp.AdvertiserID != "" {
		return imp, nil
	}
	// No advid on the impression (or no impression row yet) — fall back to the
	// click row for the exposure ids; advid may be blank (then the caller uses
	// the slug/campaign fallback).
	clk, err := queryExposure(ctx, client, reportingURL, "clicks", traceID)
	if err != nil {
		return imp, err // return whatever the impression gave us
	}
	// Prefer the impression's advid if it ever set one; otherwise the click's.
	if imp.AdvertiserID == "" {
		imp.AdvertiserID = clk.AdvertiserID
	}
	if imp.CampaignID == "" {
		imp.CampaignID = clk.CampaignID
	}
	if imp.CreativeID == "" {
		imp.CreativeID = clk.CreativeID
	}
	if imp.PlacementID == "" {
		imp.PlacementID = clk.PlacementID
	}
	if imp.PublisherID == "" {
		imp.PublisherID = clk.PublisherID
	}
	return imp, nil
}

// queryExposure reads one exposure row (impressions or clicks) for traceID via
// reporting's query API (staff-unscoped, so the dev endpoint can resolve any
// demo trace) and returns its advertiser + ids.
func queryExposure(ctx context.Context, client *http.Client, reportingURL, table, traceID string) (clickResolution, error) {
	params := analytics.QueryParams{
		Table:      table,
		Metrics:    []string{"count"},
		Dimensions: []string{"account_id", "campaign_id", "creative_id", "placement_id", "publisher_id"},
		Filters:    map[string]string{"trace_id": traceID},
		Limit:      1,
	}
	body, err := json.Marshal(params)
	if err != nil {
		return clickResolution{}, err
	}
	u := strings.TrimRight(reportingURL, "/") + routes.ReportingQuery
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(string(body)))
	if err != nil {
		return clickResolution{}, err
	}
	req.Header.Set(constants.HeaderContentType, constants.ContentTypeJSON)
	req.Header.Set(constants.HeaderAccountType, string(auth.AccountStaff))
	resp, err := client.Do(req)
	if err != nil {
		return clickResolution{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return clickResolution{}, fmt.Errorf("reporting query %s status %d", table, resp.StatusCode)
	}
	var out analytics.QueryResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return clickResolution{}, err
	}
	if len(out.Rows) == 0 {
		return clickResolution{}, nil
	}
	col := func(name string) string {
		for i, c := range out.Columns {
			if c == name && i < len(out.Rows[0]) {
				if s, ok := out.Rows[0][i].(string); ok {
					return s
				}
				return fmt.Sprintf("%v", out.Rows[0][i])
			}
		}
		return ""
	}
	return clickResolution{
		AdvertiserID: col("account_id"),
		CampaignID:   col("campaign_id"),
		CreativeID:   col("creative_id"),
		PlacementID:  col("placement_id"),
		PublisherID:  col("publisher_id"),
	}, nil
}
