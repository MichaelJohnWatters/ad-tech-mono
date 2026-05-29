// cmd/adserver serves ad creatives to end-user browsers.
// Generates tracking URLs with signed parameters via macro substitution.
package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adserving"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/health"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
)

// ServeRequest is what the exchange/SSP sends after an auction win.
type ServeRequest struct {
	TraceID      string  `json:"trace_id"`
	CampaignID   string  `json:"campaign_id"`
	CreativeID   string  `json:"creative_id"`
	PlacementID  string  `json:"placement_id"`
	PublisherID  string  `json:"publisher_id"`
	AdvertiserID string  `json:"advertiser_id"`
	IOId         string  `json:"io_id"`
	DealID       string  `json:"deal_id"`
	ClearingPrice float64 `json:"clearing_price"`
	Currency      string  `json:"currency"`
	SiteDomain    string  `json:"site_domain"`
	Width         int     `json:"width"`
	Height        int     `json:"height"`
}

// ServeResponse contains the rendered ad HTML with all macros substituted.
type ServeResponse struct {
	HTML           string `json:"html"`
	ImpressionURL  string `json:"impression_url"`
	ClickURL       string `json:"click_url"`
	ViewabilityURL string `json:"viewability_url"`
	TraceID        string `json:"trace_id"`
	CreativeID     string `json:"creative_id"`
	CampaignID     string `json:"campaign_id"`
	PlacementID    string `json:"placement_id"`
	PublisherID    string `json:"publisher_id"`
	AdvertiserID   string `json:"advertiser_id"`
	ClearingPrice  float64 `json:"clearing_price"`
	Currency       string `json:"currency"`
	Width          int    `json:"width"`
	Height         int    `json:"height"`
}

// Creative is a stored creative template.
type Creative struct {
	ID       string
	Name     string
	HTML     string // template with ${...} macros
	Width    int
	Height   int
	Format   string // banner, native, video
	LandingURL string
}

func main() {
	cfg := config.Load()
	log := logger.New("adserver")
	hlth := health.New()
	lc := lifecycle.New(log)

	port := cfg.Get("adserver.port", "8085")
	trackerURL := cfg.Get("adserver.tracker_url", "http://localhost:8083")

	creatives := seedCreatives()

	mux := http.NewServeMux()
	mux.Handle("/healthz", hlth.LivenessHandler())
	mux.Handle("/readyz", hlth.ReadinessHandler())

	// Serve an ad - called after auction win
	mux.HandleFunc("/v1/ad/serve", serveHandler(log, creatives, trackerURL))

	// List creatives for debugging
	mux.HandleFunc("/v1/ad/creatives", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(creatives)
	})

	handler := middleware.CORS(mux)
	server := &http.Server{Addr: ":" + port, Handler: handler, ReadTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second}

	log.Info("adserver starting", "port", port, "creatives", len(creatives), "tracker", trackerURL)
	lifecycle.ServeHTTP(lc, server, log, 30*time.Second)
}

func serveHandler(log *slog.Logger, creatives map[string]Creative, trackerURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req ServeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}

		ctx := logger.WithTraceID(r.Context(), req.TraceID)
		reqLog := logger.WithContext(log, ctx)

		creative, ok := creatives[req.CreativeID]
		if !ok {
			// Fallback to a generic creative
			creative = Creative{
				ID:   req.CreativeID,
				Name: "Dynamic Creative",
				HTML: defaultCreativeHTML,
			}
		}

		macroCtx := adserving.MacroContext{
			AuctionID:    req.TraceID,
			AuctionPrice: req.ClearingPrice,
			Currency:     req.Currency,
			CampaignID:   req.CampaignID,
			CreativeID:   req.CreativeID,
			PlacementID:  req.PlacementID,
			PublisherID:  req.PublisherID,
			AdvertiserID: req.AdvertiserID,
			IOId:         req.IOId,
			DealID:       req.DealID,
			SiteDomain:   req.SiteDomain,
			Width:        req.Width,
			Height:       req.Height,
			TrackerURL:   trackerURL,
		}

		// Substitute macros in creative HTML
		renderedHTML := adserving.SubstituteMacros(creative.HTML, macroCtx)

		// Build tracking URLs with full context
		impressionURL := adserving.BuildImpressionURL(macroCtx)
		clickURL := adserving.BuildClickURL(macroCtx)
		viewabilityURL := adserving.BuildViewabilityURL(macroCtx)

		resp := ServeResponse{
			HTML:           renderedHTML,
			ImpressionURL:  impressionURL,
			ClickURL:       clickURL,
			ViewabilityURL: viewabilityURL,
			TraceID:        req.TraceID,
			CreativeID:     req.CreativeID,
			CampaignID:     req.CampaignID,
			PlacementID:    req.PlacementID,
			PublisherID:    req.PublisherID,
			AdvertiserID:   req.AdvertiserID,
			ClearingPrice:  req.ClearingPrice,
			Currency:       req.Currency,
			Width:          req.Width,
			Height:         req.Height,
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)

		reqLog.Info("ad served",
			"creative", req.CreativeID,
			"campaign", req.CampaignID,
			"placement", req.PlacementID,
			"clearing_price", req.ClearingPrice,
		)
	}
}

func seedCreatives() map[string]Creative {
	return map[string]Creative{
		"cr-shoes-001": {
			ID: "cr-shoes-001", Name: "Acme Shoes - Summer Sale", Format: "banner",
			LandingURL: "https://acme-shoes.com/summer-sale",
			HTML: fmt.Sprintf(`<div style="width:${WIDTH}px;height:${HEIGHT}px;background:#f8f4f0;border:1px solid #ddd;display:flex;flex-direction:column;align-items:center;justify-content:center;font-family:sans-serif;">
  <h3 style="margin:0 0 8px;color:#1a1a2e;">Summer Shoe Sale</h3>
  <p style="margin:0 0 12px;color:#666;font-size:13px;">Up to 50%% off at Acme Shoes</p>
  <a href="${CLICK_URL}%s" style="background:#4361ee;color:white;padding:8px 20px;border-radius:4px;text-decoration:none;font-size:13px;">Shop Now</a>
  <img src="${IMP_PIXEL}" width="1" height="1" style="position:absolute;">
</div>`, "https://acme-shoes.com/summer-sale"),
		},
		"cr-shoes-002": {
			ID: "cr-shoes-002", Name: "Acme Shoes - New Collection", Format: "banner",
			LandingURL: "https://acme-shoes.com/new",
			HTML: fmt.Sprintf(`<div style="width:${WIDTH}px;height:${HEIGHT}px;background:#e8f0fe;border:1px solid #c5d5f0;display:flex;flex-direction:column;align-items:center;justify-content:center;font-family:sans-serif;">
  <h3 style="margin:0 0 8px;color:#1a1a2e;">New Collection 2024</h3>
  <p style="margin:0 0 12px;color:#666;font-size:13px;">Acme Shoes - Walk in Style</p>
  <a href="${CLICK_URL}%s" style="background:#2d6a4f;color:white;padding:8px 20px;border-radius:4px;text-decoration:none;font-size:13px;">Explore</a>
</div>`, "https://acme-shoes.com/new"),
		},
		"cr-tech-001": {
			ID: "cr-tech-001", Name: "Globex Tech - Cloud Platform", Format: "banner",
			LandingURL: "https://globex-tech.com/cloud",
			HTML: fmt.Sprintf(`<div style="width:${WIDTH}px;height:${HEIGHT}px;background:#1a1a2e;border:1px solid #333;display:flex;flex-direction:column;align-items:center;justify-content:center;font-family:sans-serif;color:white;">
  <h3 style="margin:0 0 8px;">Globex Cloud Platform</h3>
  <p style="margin:0 0 12px;color:#aaa;font-size:13px;">Scale without limits. Start free.</p>
  <a href="${CLICK_URL}%s" style="background:#818cf8;color:white;padding:8px 20px;border-radius:4px;text-decoration:none;font-size:13px;">Try Free</a>
</div>`, "https://globex-tech.com/cloud"),
		},
		"cr-saas-001": {
			ID: "cr-saas-001", Name: "Initech SaaS - Productivity", Format: "banner",
			LandingURL: "https://initech.io/signup",
			HTML: fmt.Sprintf(`<div style="width:${WIDTH}px;height:${HEIGHT}px;background:#fef3c7;border:1px solid #f59e0b;display:flex;flex-direction:column;align-items:center;justify-content:center;font-family:sans-serif;">
  <h3 style="margin:0 0 8px;color:#92400e;">Initech Productivity Suite</h3>
  <p style="margin:0 0 12px;color:#78350f;font-size:13px;">Get more done. 14-day free trial.</p>
  <a href="${CLICK_URL}%s" style="background:#f59e0b;color:white;padding:8px 20px;border-radius:4px;text-decoration:none;font-size:13px;">Start Trial</a>
</div>`, "https://initech.io/signup"),
		},
	}
}

const defaultCreativeHTML = `<div style="width:${WIDTH}px;height:${HEIGHT}px;background:#f5f5f5;border:1px solid #ddd;display:flex;flex-direction:column;align-items:center;justify-content:center;font-family:sans-serif;">
  <h3 style="margin:0 0 8px;color:#333;">Advertisement</h3>
  <p style="margin:0;color:#666;font-size:12px;">Campaign: ${CAMPAIGN_ID}</p>
  <p style="margin:0;color:#666;font-size:12px;">Creative: ${CREATIVE_ID}</p>
</div>`
