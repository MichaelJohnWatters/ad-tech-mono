// cmd/ssp is the Supply-Side Platform service.
// Manages publisher inventory, generates bid requests.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/health"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
)

// Placement represents a publisher's ad slot.
type Placement struct {
	ID          string  `json:"id"`
	PublisherID string  `json:"publisher_id"`
	SiteDomain  string  `json:"site_domain"`
	PageURL     string  `json:"page_url"`
	Format      string  `json:"format"` // banner, video, native
	Width       int     `json:"width"`
	Height      int     `json:"height"`
	FloorPrice  float64 `json:"floor_price"`
	Categories  []string `json:"categories"`
}

func main() {
	cfg := config.Load()
	log := logger.New("ssp")
	hlth := health.New()
	lc := lifecycle.New(log)

	port := cfg.Get("ssp.port", "8084")
	exchangeURL := cfg.Get("ssp.exchange_url", "http://localhost:8081")

	placements := seedPlacements()

	mux := http.NewServeMux()
	mux.Handle("/healthz", hlth.LivenessHandler())
	mux.Handle("/readyz", hlth.ReadinessHandler())

	// List placements
	mux.HandleFunc("/v1/ssp/placements", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(placements)
	})

	// Request an ad for a placement - generates a bid request and sends to exchange
	mux.HandleFunc("/v1/ssp/request", requestAdHandler(log, placements, exchangeURL))

	handler := middleware.CORS(mux)
	server := &http.Server{Addr: ":" + port, Handler: handler, ReadTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second}

	log.Info("ssp starting", "port", port, "placements", len(placements), "exchange", exchangeURL)
	lifecycle.ServeHTTP(lc, server, log, 30*time.Second)
}

func requestAdHandler(log *slog.Logger, placements []Placement, exchangeURL string) http.HandlerFunc {
	placementMap := make(map[string]Placement)
	for _, p := range placements {
		placementMap[p.ID] = p
	}

	return func(w http.ResponseWriter, r *http.Request) {
		placementID := r.URL.Query().Get("placement_id")
		geo := r.URL.Query().Get("geo")
		device := r.URL.Query().Get("device")
		userID := r.URL.Query().Get("user_id")

		if placementID == "" {
			placementID = "pl-news-mpu" // default
		}
		p, ok := placementMap[placementID]
		if !ok {
			http.Error(w, "placement not found", http.StatusNotFound)
			return
		}

		traceID := fmt.Sprintf("ssp-%d", time.Now().UnixMilli())
		ctx := logger.WithTraceID(r.Context(), traceID)
		reqLog := logger.WithContext(log, ctx)

		// Build OpenRTB bid request
		bidReq := openrtb.BidRequest{
			ID: traceID,
			Imp: []openrtb.Imp{{
				ID:       "imp-1",
				BidFloor: p.FloorPrice,
			}},
			Site: &openrtb.Site{
				Domain: p.SiteDomain,
				Page:   p.PageURL,
				Cat:    p.Categories,
			},
			TMax: 100,
		}

		if p.Format == "banner" || p.Format == "" {
			bidReq.Imp[0].Banner = &openrtb.Banner{W: p.Width, H: p.Height}
		}

		if geo != "" {
			if bidReq.Device == nil {
				bidReq.Device = &openrtb.Device{}
			}
			bidReq.Device.Geo = &openrtb.Geo{Country: geo}
		}
		if device != "" {
			if bidReq.Device == nil {
				bidReq.Device = &openrtb.Device{}
			}
			bidReq.Device.DeviceType = deviceTypeInt(device)
		}
		if userID != "" {
			bidReq.User = &openrtb.User{ID: userID}
		}

		reqLog.Info("bid request generated",
			"placement", p.ID,
			"publisher", p.PublisherID,
			"floor", p.FloorPrice,
			"size", fmt.Sprintf("%dx%d", p.Width, p.Height),
		)

		// Send to exchange
		body, _ := json.Marshal(bidReq)
		resp, err := http.Post(exchangeURL+"/v1/openrtb/auction", "application/json", bytes.NewReader(body))
		if err != nil {
			reqLog.Error("exchange call failed", "error", err)
			http.Error(w, "exchange unavailable", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		var bidResp openrtb.BidResponse
		json.NewDecoder(resp.Body).Decode(&bidResp)

		// Enrich response with placement context for the ad server
		result := map[string]interface{}{
			"trace_id":     traceID,
			"placement_id": p.ID,
			"publisher_id": p.PublisherID,
			"site_domain":  p.SiteDomain,
			"width":        p.Width,
			"height":       p.Height,
			"bid_response": bidResp,
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	}
}

func seedPlacements() []Placement {
	return []Placement{
		{
			ID: "pl-news-mpu", PublisherID: "pub-daily-news", SiteDomain: "daily-news.com",
			PageURL: "https://daily-news.com/article/123", Format: "banner",
			Width: 300, Height: 250, FloorPrice: 1.00,
			Categories: []string{"IAB12"}, // News
		},
		{
			ID: "pl-news-leaderboard", PublisherID: "pub-daily-news", SiteDomain: "daily-news.com",
			PageURL: "https://daily-news.com/", Format: "banner",
			Width: 728, Height: 90, FloorPrice: 1.50,
			Categories: []string{"IAB12"},
		},
		{
			ID: "pl-tech-sidebar", PublisherID: "pub-tech-review", SiteDomain: "tech-review.io",
			PageURL: "https://tech-review.io/reviews", Format: "banner",
			Width: 160, Height: 600, FloorPrice: 0.80,
			Categories: []string{"IAB19"}, // Technology
		},
		{
			ID: "pl-sport-mpu", PublisherID: "pub-sports-daily", SiteDomain: "sports-daily.com",
			PageURL: "https://sports-daily.com/live", Format: "banner",
			Width: 300, Height: 250, FloorPrice: 2.00,
			Categories: []string{"IAB17"}, // Sports
		},
		{
			ID: "pl-shop-billboard", PublisherID: "pub-shoppers-hub", SiteDomain: "shoppers-hub.com",
			PageURL: "https://shoppers-hub.com/deals", Format: "banner",
			Width: 970, Height: 250, FloorPrice: 3.00,
			Categories: []string{"IAB22"}, // Shopping
		},
	}
}

func deviceTypeInt(s string) int {
	switch s {
	case "mobile":
		return 1
	case "desktop":
		return 2
	case "ctv":
		return 3
	case "tablet":
		return 5
	default:
		return 2
	}
}

