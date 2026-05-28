// cmd/dsp is the Demand-Side Platform service.
// Manages campaigns, evaluates bid requests, submits bids.
package main

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/health"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/pacing"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/targeting"
)

func main() {
	clk := clock.Real{}
	cfg := config.Load()
	slog := logger.New("dsp")
	hlth := health.New()
	lc := lifecycle.New(slog)

	port := cfg.Get("dsp.port", "8082")

	mux := http.NewServeMux()
	mux.Handle("/healthz", hlth.LivenessHandler())
	mux.Handle("/readyz", hlth.ReadinessHandler())

	// OpenRTB bid endpoint - Exchange calls this
	mux.HandleFunc("/v1/openrtb/bid", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var bidReq openrtb.BidRequest
		if err := json.NewDecoder(r.Body).Decode(&bidReq); err != nil {
			http.Error(w, "invalid bid request", http.StatusBadRequest)
			return
		}

		ctx := logger.WithTraceID(r.Context(), bidReq.ID)
		reqLog := logger.WithContext(slog, ctx)

		// TODO(phase2-wire): Load active campaigns from cache, evaluate targeting
		// For now: simple static bid for testing

		// Build targeting request from OpenRTB
		tReq := targeting.Request{
			Device:        deviceTypeStr(bidReq.Device),
			InventoryType: inventoryType(bidReq),
		}
		if bidReq.Device != nil && bidReq.Device.Geo != nil {
			tReq.Geo = bidReq.Device.Geo.Country
		}
		if bidReq.Site != nil {
			tReq.Domain = bidReq.Site.Domain
			tReq.Categories = bidReq.Site.Cat
		}
		if bidReq.User != nil && bidReq.User.Ext != nil {
			tReq.Segments = bidReq.User.Ext.Segments
		}

		// Simple demo: always bid $2.00 CPM (will be replaced with real campaign logic)
		demoRules := targeting.Rules{} // empty = match everything
		result := targeting.Evaluate(demoRules, tReq)
		if !result.Matched {
			// No bid
			resp := openrtb.BidResponse{ID: bidReq.ID, NoBid: true}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}

		// Pacing check
		demoPacer := pacing.New(clk, pacing.Config{
			Mode:        pacing.ModeEven,
			DailyBudget: 1000.0,
			DayStartUTC: time.Now().Truncate(24 * time.Hour),
		})
		if !demoPacer.ShouldBid(0) { // TODO: real spend tracking
			resp := openrtb.BidResponse{ID: bidReq.ID, NoBid: true}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}

		// Submit bid
		baseBid := 2.00
		mods := targeting.Modifiers{
			Device: map[string]float64{"mobile": 20},
		}
		modCtx := targeting.ModifierContext{Device: tReq.Device}
		adjustedBid, _ := targeting.ApplyModifiers(baseBid, mods, modCtx)

		resp := openrtb.BidResponse{
			ID:  bidReq.ID,
			Cur: "USD",
			SeatBid: []openrtb.SeatBid{
				{
					Seat: "dsp-internal",
					Bid: []openrtb.BidObj{
						{
							ID:      "bid-" + bidReq.ID,
							ImpID:   bidReq.Imp[0].ID,
							Price:   adjustedBid,
							CID:     "demo-campaign",
							CrID:    "demo-creative",
							ADomain: []string{"demo-advertiser.com"},
						},
					},
				},
			},
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)

		reqLog.Info("bid submitted", "price", adjustedBid, "campaign", "demo-campaign")
	})

	server := &http.Server{
		Addr:         ":" + port,
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	slog.Info("dsp starting", "port", port)
	lifecycle.ServeHTTP(lc, server, slog, 30*time.Second)
}

func deviceTypeStr(d *openrtb.Device) string {
	if d == nil {
		return "desktop"
	}
	switch d.DeviceType {
	case 1:
		return "mobile"
	case 2:
		return "desktop"
	case 3:
		return "ctv"
	case 5:
		return "tablet"
	default:
		return "desktop"
	}
}

func inventoryType(req openrtb.BidRequest) string {
	if req.App != nil {
		return "app"
	}
	return "site"
}
