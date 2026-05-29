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
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/health"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/models"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/optimise"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// AdCreative is the ad server's internal creative with HTML template.
// Different from models.Creative which is the database model.
type AdCreative struct {
	ID         string
	Name       string
	HTML       string // template with ${...} macros
	Width      int
	Height     int
	Format     string
	LandingURL string
}

func main() {
	log := logger.New(constants.ServiceAdServer)
	sc := config.Setup(constants.ServiceAdServer, log)
	cfg := sc.Cfg
	_ = sc
	hlth := health.New()
	lc := lifecycle.New(log)

	port := cfg.Get("adserver.port", routes.PortAdServer)
	trackerURL := cfg.Get("adserver.tracker_url", routes.DefaultTrackerURL)

	creatives := seedCreatives()

	// Creative rotation bandit (Thompson Sampling)
	creativeIDs := make([]string, 0, len(creatives))
	for id := range creatives {
		creativeIDs = append(creativeIDs, id)
	}
	bandit := optimise.NewBandit(creativeIDs)

	mux := http.NewServeMux()
	mux.Handle(routes.Healthz, hlth.LivenessHandler())
	mux.Handle(routes.Readyz, hlth.ReadinessHandler())

	// Serve an ad - called after auction win
	mux.HandleFunc(routes.AdServe, serveHandler(log, creatives, trackerURL))

	// Bandit stats for debugging
	mux.HandleFunc(routes.AdBandit, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"stats":   bandit.Stats(),
			"weights": bandit.Weights(),
		})
	})

	// List creatives for debugging
	mux.HandleFunc(routes.AdCreatives, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		json.NewEncoder(w).Encode(creatives)
	})

	handler := middleware.CORS(mux)
	server := &http.Server{Addr: ":" + port, Handler: handler, ReadTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second}

	log.Info("adserver starting", "port", port, "creatives", len(creatives), "tracker", trackerURL)
	lifecycle.ServeHTTP(lc, server, log, 30*time.Second)
}

func serveHandler(log *slog.Logger, creatives map[string]AdCreative, trackerURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req models.ServeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}

		ctx := logger.WithTraceID(r.Context(), req.TraceID)
		reqLog := logger.WithContext(log, ctx)

		creative, ok := creatives[req.CreativeID]
		if !ok {
			// Fallback to a generic creative
			creative = AdCreative{
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

		resp := models.ServeResponse{
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

		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		json.NewEncoder(w).Encode(resp)

		reqLog.Info("ad served",
			"creative", req.CreativeID,
			"campaign", req.CampaignID,
			"placement", req.PlacementID,
			"clearing_price", req.ClearingPrice,
		)
	}
}

func seedCreatives() map[string]AdCreative {
	return map[string]AdCreative{
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
