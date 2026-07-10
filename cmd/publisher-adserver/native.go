package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adserving"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/native"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tracing"
)

// nativeHandler renders an OpenRTB Native 1.2 winner into a browser-viewable
// HTML fragment with signed trackers. Flow (mirrors vastHandler):
//
//  1. Browser fetches /v1/pubad/native?placement_id=...
//  2. We call SSP /v1/ssp/serve with channel=native — the SSP runs the auction
//     and returns the winner, including the DSP's native response in AdM.
//  3. We parse the native response (pkg/native), sign impression + click
//     tracker URLs from the winner's identifiers (same HMAC pipeline as
//     display/video), and lay the assets out as an HTML card.
//  4. The impression pixel fires on render; the whole card links through the
//     signed click tracker, which 302s to the landing URL.
//
// On any failure (SSP unreachable, no bid, unparseable markup) we render a
// demo native ad so the simulator never sees a broken slot.
func nativeHandler(log *slog.Logger, trackerURL, sspURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		traceID := tracing.TraceIDFromContext(ctx)
		if traceID == "" {
			traceID = fmt.Sprintf("native-%d", time.Now().UnixMilli())
		}
		reqLog := logger.WithContext(log, logger.WithTraceID(ctx, traceID))

		placementID := r.URL.Query().Get("placement_id")
		if placementID == "" {
			placementID = "pl-news-mpu" // demo default
		}

		winner, err := fetchNativeWinner(ctx, sspURL, placementID, traceID, r.URL.Query())
		var resp native.Response
		var macroCtx adserving.MacroContext
		if err != nil || winner == nil || winner.NoBid || winner.AdM == "" {
			if err != nil {
				reqLog.Warn("native auction failed, serving demo native", "error", err)
			} else {
				reqLog.Info("native auction: no bid / no markup, serving demo native")
			}
			resp, macroCtx = stubNative(trackerURL, traceID, placementID)
		} else {
			parsed, perr := native.ParseResponse(winner.AdM)
			if perr != nil {
				reqLog.Warn("native markup unparseable, serving demo native", "error", perr)
				resp, macroCtx = stubNative(trackerURL, traceID, placementID)
			} else {
				resp = parsed
				macroCtx = nativeMacroCtx(winner, trackerURL, resp.Native.Link.URL)
				reqLog.Info("native bid served",
					"trace_id", winner.TraceID,
					"creative", winner.CreativeID,
					"advertiser", winner.AdvertiserDomain,
					"price", winner.ClearingPrice)
			}
		}

		html, err := renderNativeHTML(resp, macroCtx)
		if err != nil {
			reqLog.Error("native render failed", "error", err)
			http.Error(w, "native render failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write([]byte(html))
	}
}

// fetchNativeWinner calls SSP /v1/ssp/serve?channel=native. Returns nil on
// no-bid; error only on transport/decode failure.
func fetchNativeWinner(ctx context.Context, sspURL, placementID, traceID string, incoming url.Values) (*sspVideoWinner, error) {
	q := forwardSSPQuery(incoming, "native", placementID, "mobile")
	target := sspURL + routes.SSPServe + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("build ssp request: %w", err)
	}
	tracing.InjectHTTP(ctx, req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ssp call: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ssp body read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ssp returned %d: %s", resp.StatusCode, string(body))
	}
	var winner sspVideoWinner
	if err := json.Unmarshal(body, &winner); err != nil {
		return nil, fmt.Errorf("ssp decode: %w", err)
	}
	return &winner, nil
}

// nativeMacroCtx builds the signing context for a native winner. LandingURL
// comes from the native response's link (falling back to the advertiser's demo
// landing page) so the signed click tracker redirects there.
func nativeMacroCtx(winner *sspVideoWinner, trackerURL, linkURL string) adserving.MacroContext {
	landing := linkURL
	if landing == "" {
		landing = landingForDomain(winner.AdvertiserDomain)
	}
	return adserving.MacroContext{
		AuctionID:    defaultStr2(winner.TraceID, "native"),
		AuctionPrice: winner.ClearingPrice,
		Currency:     defaultStr2(winner.Currency, "USD"),
		CampaignID:   winner.CampaignID,
		CreativeID:   winner.CreativeID,
		PlacementID:  winner.PlacementID,
		PublisherID:  winner.PublisherID,
		AdvertiserID: winner.AdvertiserID,
		BidModel:     defaultStr2(winner.BidModel, "cpm"),
		DealID:       winner.DealID,
		TrackerURL:   trackerURL,
		LandingURL:   landing,
		URLTTL:       time.Hour,
	}
}

// nativeView is the flattened asset set the template renders.
type nativeView struct {
	Title         string
	MainImage     string
	Icon          string
	Sponsored     string
	Body          string
	CTA           string
	ClickURL      string
	ImpressionURL string
}

// nativeTemplate lays out a native card: sponsored label, main image, title,
// body, and a CTA button, all wrapped in the signed click link, with the
// impression pixel appended. Inline styles keep it a self-contained fragment.
var nativeTemplate = template.Must(template.New("native").Parse(`<div class="adtech-native" style="max-width:400px;border:1px solid #e2e2e2;border-radius:8px;overflow:hidden;font-family:system-ui,sans-serif">
  <a href="{{.ClickURL}}" target="_blank" rel="noopener nofollow" style="text-decoration:none;color:inherit;display:block">
    {{if .MainImage}}<img src="{{.MainImage}}" alt="{{.Title}}" style="width:100%;height:auto;display:block">{{end}}
    <div style="padding:12px">
      {{if .Sponsored}}<div style="font-size:11px;text-transform:uppercase;letter-spacing:.04em;color:#888">{{if .Icon}}<img src="{{.Icon}}" alt="" style="width:16px;height:16px;vertical-align:middle;margin-right:4px;border-radius:3px">{{end}}Sponsored · {{.Sponsored}}</div>{{end}}
      {{if .Title}}<div style="font-size:16px;font-weight:600;margin:6px 0">{{.Title}}</div>{{end}}
      {{if .Body}}<div style="font-size:13px;color:#555;line-height:1.4">{{.Body}}</div>{{end}}
      {{if .CTA}}<span style="display:inline-block;margin-top:10px;padding:8px 14px;background:#1a73e8;color:#fff;border-radius:6px;font-size:13px;font-weight:600">{{.CTA}}</span>{{end}}
    </div>
  </a>
  <img src="{{.ImpressionURL}}" width="1" height="1" style="position:absolute;left:-9999px" alt="" aria-hidden="true">
</div>`))

// renderNativeHTML flattens the native response assets (by their standard IDs)
// and renders the HTML card with signed trackers.
func renderNativeHTML(resp native.Response, macroCtx adserving.MacroContext) (string, error) {
	v := nativeView{
		ClickURL:      adserving.BuildClickURL(macroCtx),
		ImpressionURL: adserving.BuildImpressionURL(macroCtx),
	}
	for _, a := range resp.Native.Assets {
		switch a.ID {
		case native.AssetIDTitle:
			if a.Title != nil {
				v.Title = a.Title.Text
			}
		case native.AssetIDMainImage:
			if a.Img != nil {
				v.MainImage = a.Img.URL
			}
		case native.AssetIDIcon:
			if a.Img != nil {
				v.Icon = a.Img.URL
			}
		case native.AssetIDSponsored:
			if a.Data != nil {
				v.Sponsored = a.Data.Value
			}
		case native.AssetIDBody:
			if a.Data != nil {
				v.Body = a.Data.Value
			}
		case native.AssetIDCTA:
			if a.Data != nil {
				v.CTA = a.Data.Value
			}
		}
	}
	var b strings.Builder
	if err := nativeTemplate.Execute(&b, v); err != nil {
		return "", err
	}
	return b.String(), nil
}

// stubNative is the demo fallback: a fixed native ad + macro context so the
// simulator always sees a rendered slot even with no live bid.
func stubNative(trackerURL, traceID, placementID string) (native.Response, adserving.MacroContext) {
	macroCtx := adserving.MacroContext{
		AuctionID:    traceID,
		AuctionPrice: 3.50,
		Currency:     "USD",
		CampaignID:   "demo-native-li",
		CreativeID:   "demo-native-cr",
		PlacementID:  placementID,
		PublisherID:  "demo-pub",
		AdvertiserID: "demo-acme",
		BidModel:     "cpm",
		TrackerURL:   trackerURL,
		LandingURL:   "http://localhost:8080/dev/landing/acme-shoes",
		URLTTL:       time.Hour,
	}
	resp := native.BuildResponse(native.AssetSet{
		Title:      "Acme Running Shoes — 20% Off",
		MainImage:  "http://localhost:8080/v1/creatives/themes/retail-1200x627.svg",
		Sponsored:  "Acme",
		Body:       "Lightweight, all-day comfort. Free returns on every pair.",
		CTA:        "Shop now",
		LandingURL: macroCtx.LandingURL,
	}, nil, nil)
	return resp, macroCtx
}
