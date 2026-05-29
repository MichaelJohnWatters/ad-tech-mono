// cmd/dsp is the Demand-Side Platform service.
// Manages campaigns, evaluates bid requests, submits bids.
package main

import (
	"encoding/json"
	"log/slog"
	"math/rand"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/bidshading"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/health"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/pacing"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/targeting"
)

// Campaign is an active campaign with targeting, budget, and creative info.
type Campaign struct {
	ID             string
	AccountID      string
	AdvertiserID   string
	IOId           string
	Name           string
	CreativeID     string
	CreativeDomain string
	BaseBid        float64
	Currency       string
	DailyBudget    float64
	TotalBudget    float64
	BidModel       string // cpm, cpc, cpa
	PacingMode     string // even, asap, front_loaded
	Status         string // live, paused, ended
	Targeting      targeting.Rules
	Modifiers      targeting.Modifiers
}

// BudgetTracker tracks spend per campaign in memory.
// In production this would be Redis-backed.
type BudgetTracker struct {
	mu    sync.RWMutex
	spend map[string]float64 // campaign_id -> today's spend
}

func NewBudgetTracker() *BudgetTracker {
	return &BudgetTracker{spend: make(map[string]float64)}
}

func (b *BudgetTracker) Spend(campaignID string) float64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.spend[campaignID]
}

func (b *BudgetTracker) Record(campaignID string, amount float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.spend[campaignID] += amount
}

func main() {
	clk := clock.Real{}
	log := logger.New(constants.ServiceDSP)
	sc := config.Setup(constants.ServiceDSP, log)
	cfg := sc.Cfg
	_ = sc
	hlth := health.New()
	lc := lifecycle.New(log)

	port := cfg.Get("dsp.port", routes.PortDSP)
	profile := cfg.Get("dsp.profile", "internal")

	// Load campaigns from YAML profile
	dspProfile, err := FindProfile(profile, log)
	if err != nil {
		log.Warn("profile YAML not found, using hardcoded fallback", "profile", profile, "error", err)
		dspProfile = &DSPProfile{Name: profile, Competitor: profile != "internal", NoisePct: 30, NoBidRate: 0.20}
		dspProfile.Campaigns = nil // will use hardcoded fallback
	}

	var campaigns []Campaign
	if len(dspProfile.Campaigns) > 0 {
		campaigns = ProfileToCampaigns(dspProfile)
	} else {
		campaigns = loadCampaignProfile(profile) // hardcoded fallback
	}
	budget := NewBudgetTracker()
	shadingTracker := bidshading.NewTracker()
	isCompetitor := dspProfile.Competitor
	noisePct := dspProfile.NoisePct
	noBidRate := dspProfile.NoBidRate

	mux := http.NewServeMux()
	mux.Handle(routes.Healthz, hlth.LivenessHandler())
	mux.Handle(routes.Readyz, hlth.ReadinessHandler())
	mux.HandleFunc(routes.OpenRTBBid, bidHandler(log, clk, campaigns, budget, isCompetitor, noisePct, noBidRate))

	// Win/loss notification endpoints
	mux.HandleFunc(routes.OpenRTBWin, winHandler(log, budget, shadingTracker))
	mux.HandleFunc(routes.OpenRTBLoss, lossHandler(log, shadingTracker))

	// Win-rate stats endpoint for debugging
	mux.HandleFunc(routes.DSPShading, func(w http.ResponseWriter, r *http.Request) {
		placements := shadingTracker.AllPlacements()
		result := make(map[string]bidshading.PlacementStats)
		for _, pid := range placements {
			result[pid] = shadingTracker.Stats(pid)
		}
		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		json.NewEncoder(w).Encode(result)
	})

	// Campaign list endpoint for debugging
	mux.HandleFunc(routes.DSPCampaigns, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		json.NewEncoder(w).Encode(campaigns)
	})

	handler := middleware.CORS(mux)
	server := &http.Server{Addr: ":" + port, Handler: handler, ReadTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second}

	log.Info("dsp starting", "port", port, "profile", profile, "campaigns", len(campaigns))
	lifecycle.ServeHTTP(lc, server, log, 30*time.Second)
}

func bidHandler(log *slog.Logger, clk clock.Clock, campaigns []Campaign, budget *BudgetTracker, isCompetitor bool, noisePct, noBidRate float64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
		reqLog := logger.WithContext(log, ctx)

		// Build targeting context from bid request
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
			// Contextual classification: if publisher didn't declare categories,
			// classify from URL pattern or keywords
			if len(tReq.Categories) == 0 {
				classifier := targeting.NewClassifier()
				tReq.Categories = classifier.Classify(
					bidReq.Site.Domain, bidReq.Site.Page,
					nil, nil,
				)
			}
		}
		if bidReq.User != nil && bidReq.User.Ext != nil {
			tReq.Segments = bidReq.User.Ext.Segments
		}

		floor := 0.0
		if len(bidReq.Imp) > 0 {
			floor = bidReq.Imp[0].BidFloor
		}

		// Evaluate each campaign
		var bestBid *openrtb.BidObj
		var bestCampaign *Campaign
		var bestPrice float64

		for i := range campaigns {
			c := &campaigns[i]
			if c.Status != constants.StatusLive {
				continue
			}

			// Targeting check
			result := targeting.Evaluate(c.Targeting, tReq)
			if !result.Matched {
				reqLog.Debug("campaign excluded by targeting", "campaign", c.ID, "dimension", result.FailedDimension)
				continue
			}

			// Pacing check
			pacer := pacing.New(clk, pacing.Config{
				Mode:        pacingMode(c.PacingMode),
				DailyBudget: c.DailyBudget,
				DayStartUTC: clk.Now().Truncate(24 * time.Hour),
			})
			currentSpend := budget.Spend(c.ID)
			if !pacer.ShouldBid(currentSpend) {
				reqLog.Debug("campaign throttled by pacing", "campaign", c.ID, "spend", currentSpend)
				continue
			}

			// Budget check
			if currentSpend >= c.DailyBudget {
				reqLog.Debug("campaign daily budget exhausted", "campaign", c.ID)
				continue
			}

			// Apply bid modifiers
			modCtx := targeting.ModifierContext{
				Device:     tReq.Device,
				GeoCountry: tReq.Geo,
			}
			adjustedBid, _ := targeting.ApplyModifiers(c.BaseBid, c.Modifiers, modCtx)

			// Competitor DSPs: add random noise to simulate unpredictable bidding.
			// noise_pct and no_bid_rate are configurable per profile YAML.
			if isCompetitor {
				if rand.Float64() < noBidRate {
					reqLog.Debug("competitor random no-bid", "campaign", c.ID)
					continue
				}
				noiseFraction := noisePct / 100.0
				noiseMin := 1.0 - noiseFraction
				noiseRange := noiseFraction * 2.0
				adjustedBid *= noiseMin + rand.Float64()*noiseRange
			}

			// Floor check
			if adjustedBid < floor {
				reqLog.Debug("bid below floor", "campaign", c.ID, "bid", adjustedBid, "floor", floor)
				continue
			}

			// Pick highest bidder
			if adjustedBid > bestPrice {
				bestPrice = adjustedBid
				bestCampaign = c
				bestBid = &openrtb.BidObj{
					ID:      "bid-" + bidReq.ID + "-" + c.ID,
					ImpID:   bidReq.Imp[0].ID,
					Price:   adjustedBid,
					CID:     c.ID,
					CrID:    c.CreativeID,
					ADomain: []string{c.CreativeDomain},
				}
			}
		}

		if bestBid == nil {
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
			json.NewEncoder(w).Encode(openrtb.BidResponse{ID: bidReq.ID, NoBid: true})
			reqLog.Info("no bid", "reason", "no eligible campaigns")
			return
		}

		// Record the bid (budget will be decremented on win notification)
		resp := openrtb.BidResponse{
			ID:  bidReq.ID,
			Cur: bestCampaign.Currency,
			SeatBid: []openrtb.SeatBid{{
				Seat: bestCampaign.AccountID,
				Bid:  []openrtb.BidObj{*bestBid},
			}},
		}

		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		json.NewEncoder(w).Encode(resp)

		reqLog.Info("bid submitted",
			"campaign", bestCampaign.ID,
			"campaign_name", bestCampaign.Name,
			"price", bestPrice,
			"creative", bestCampaign.CreativeID,
		)
	}
}

// loadCampaignProfile returns campaigns for the given DSP profile.
func loadCampaignProfile(profile string) []Campaign {
	switch profile {
	case "competitor1":
		return competitor1Campaigns()
	case "competitor2":
		return competitor2Campaigns()
	default:
		return internalCampaigns()
	}
}

// internalCampaigns - our platform's advertisers.
func internalCampaigns() []Campaign {
	return []Campaign{
		{
			ID: "li-001", AccountID: "adv-acme", AdvertiserID: "adv-acme",
			IOId: "io-001", Name: "Acme Shoes - UK Mobile",
			CreativeID: "cr-shoes-001", CreativeDomain: "acme-shoes.com",
			BaseBid: 2.50, Currency: "USD", DailyBudget: 500, TotalBudget: 10000,
			BidModel: constants.BidModelCPM, PacingMode: constants.PacingEven, Status: constants.StatusLive,
			Targeting: targeting.Rules{
				Include: targeting.TargetingSet{Geo: []string{"GBR"}, Device: []string{"mobile"}},
			},
		},
		{
			ID: "li-002", AccountID: "adv-acme", AdvertiserID: "adv-acme",
			IOId: "io-001", Name: "Acme Shoes - US All Devices",
			CreativeID: "cr-shoes-002", CreativeDomain: "acme-shoes.com",
			BaseBid: 3.00, Currency: "USD", DailyBudget: 1000, TotalBudget: 25000,
			BidModel: constants.BidModelCPM, PacingMode: constants.PacingEven, Status: constants.StatusLive,
			Targeting: targeting.Rules{
				Include: targeting.TargetingSet{Geo: []string{"USA"}},
			},
			Modifiers: targeting.Modifiers{
				Device: map[string]float64{"mobile": 15, "tablet": -10},
			},
		},
		{
			ID: "li-003", AccountID: "adv-globex", AdvertiserID: "adv-globex",
			IOId: "io-002", Name: "Globex Tech - Desktop Worldwide",
			CreativeID: "cr-tech-001", CreativeDomain: "globex-tech.com",
			BaseBid: 1.80, Currency: "USD", DailyBudget: 300, TotalBudget: 5000,
			BidModel: constants.BidModelCPM, PacingMode: constants.PacingEven, Status: constants.StatusLive,
			Targeting: targeting.Rules{
				Include: targeting.TargetingSet{Device: []string{"desktop"}},
			},
			Modifiers: targeting.Modifiers{
				GeoCountry: map[string]float64{"USA": 20, "GBR": 10},
			},
		},
		{
			ID: "li-004", AccountID: "adv-initech", AdvertiserID: "adv-initech",
			IOId: "io-003", Name: "Initech SaaS - Run of Network",
			CreativeID: "cr-saas-001", CreativeDomain: "initech.io",
			BaseBid: 1.20, Currency: "USD", DailyBudget: 200, TotalBudget: 3000,
			BidModel: constants.BidModelCPM, PacingMode: constants.PacingASAP, Status: constants.StatusLive,
			Targeting: targeting.Rules{},
			Modifiers: targeting.Modifiers{
				Device:     map[string]float64{"mobile": 25},
				GeoCountry: map[string]float64{"USA": 15, "GBR": 10, "DEU": 5},
			},
		},
	}
}

// competitor1Campaigns - a large retail advertiser agency. Bids aggressively on mobile.
func competitor1Campaigns() []Campaign {
	return []Campaign{
		{
			ID: "c1-001", AccountID: "agency-omnicom", AdvertiserID: "adv-megastore",
			IOId: "c1-io-001", Name: "MegaStore Summer Blowout",
			CreativeID: "c1-cr-001", CreativeDomain: "megastore.com",
			BaseBid: 3.50, Currency: "USD", DailyBudget: 5000, TotalBudget: 100000,
			BidModel: constants.BidModelCPM, PacingMode: constants.PacingASAP, Status: constants.StatusLive,
			Targeting: targeting.Rules{
				Include: targeting.TargetingSet{Geo: []string{"GBR", "USA", "DEU", "FRA"}},
			},
			Modifiers: targeting.Modifiers{
				Device:     map[string]float64{"mobile": 30},
				GeoCountry: map[string]float64{"USA": 25, "GBR": 15},
			},
		},
		{
			ID: "c1-002", AccountID: "agency-omnicom", AdvertiserID: "adv-luxauto",
			IOId: "c1-io-002", Name: "LuxAuto - Premium Desktop",
			CreativeID: "c1-cr-002", CreativeDomain: "luxauto.com",
			BaseBid: 8.00, Currency: "USD", DailyBudget: 2000, TotalBudget: 50000,
			BidModel: constants.BidModelCPM, PacingMode: constants.PacingEven, Status: constants.StatusLive,
			Targeting: targeting.Rules{
				Include: targeting.TargetingSet{Device: []string{"desktop"}, Geo: []string{"GBR", "USA"}},
			},
		},
		{
			ID: "c1-003", AccountID: "agency-omnicom", AdvertiserID: "adv-fastfood",
			IOId: "c1-io-003", Name: "QuickBite App Install",
			CreativeID: "c1-cr-003", CreativeDomain: "quickbite.app",
			BaseBid: 1.50, Currency: "USD", DailyBudget: 3000, TotalBudget: 60000,
			BidModel: constants.BidModelCPM, PacingMode: constants.PacingFrontLoaded, Status: constants.StatusLive,
			Targeting: targeting.Rules{
				Include: targeting.TargetingSet{Device: []string{"mobile", "tablet"}},
			},
			Modifiers: targeting.Modifiers{
				GeoCountry: map[string]float64{"USA": 40, "GBR": 20},
			},
		},
	}
}

// competitor2Campaigns - a performance-focused DSP. Lower bids but high volume.
func competitor2Campaigns() []Campaign {
	return []Campaign{
		{
			ID: "c2-001", AccountID: "dsp-perf-media", AdvertiserID: "adv-vpn-plus",
			IOId: "c2-io-001", Name: "VPN Plus - Global RON",
			CreativeID: "c2-cr-001", CreativeDomain: "vpnplus.com",
			BaseBid: 0.80, Currency: "USD", DailyBudget: 10000, TotalBudget: 200000,
			BidModel: constants.BidModelCPM, PacingMode: constants.PacingASAP, Status: constants.StatusLive,
			Targeting: targeting.Rules{}, // run of network
			Modifiers: targeting.Modifiers{
				Device:     map[string]float64{"mobile": 10, "desktop": 5},
				GeoCountry: map[string]float64{"USA": 50, "GBR": 30, "DEU": 20, "FRA": 15, "JPN": 10},
			},
		},
		{
			ID: "c2-002", AccountID: "dsp-perf-media", AdvertiserID: "adv-crypto-ex",
			IOId: "c2-io-002", Name: "CryptoEx - High Value Geo",
			CreativeID: "c2-cr-002", CreativeDomain: "cryptoex.io",
			BaseBid: 5.00, Currency: "USD", DailyBudget: 1000, TotalBudget: 20000,
			BidModel: constants.BidModelCPM, PacingMode: constants.PacingEven, Status: constants.StatusLive,
			Targeting: targeting.Rules{
				Include: targeting.TargetingSet{Geo: []string{"USA", "GBR"}, Device: []string{"desktop"}},
			},
		},
		{
			ID: "c2-003", AccountID: "dsp-perf-media", AdvertiserID: "adv-game-studio",
			IOId: "c2-io-003", Name: "Epic Quest - Mobile Gamers",
			CreativeID: "c2-cr-003", CreativeDomain: "epicquest.game",
			BaseBid: 4.00, Currency: "USD", DailyBudget: 4000, TotalBudget: 80000,
			BidModel: constants.BidModelCPM, PacingMode: constants.PacingASAP, Status: constants.StatusLive,
			Targeting: targeting.Rules{
				Include: targeting.TargetingSet{Device: []string{"mobile"}},
			},
			Modifiers: targeting.Modifiers{
				GeoCountry: map[string]float64{"USA": 20, "JPN": 35, "GBR": 10},
			},
		},
		{
			ID: "c2-004", AccountID: "dsp-perf-media", AdvertiserID: "adv-saas-crm",
			IOId: "c2-io-004", Name: "CloudCRM - B2B Desktop",
			CreativeID: "c2-cr-004", CreativeDomain: "cloudcrm.io",
			BaseBid: 6.50, Currency: "USD", DailyBudget: 800, TotalBudget: 15000,
			BidModel: constants.BidModelCPM, PacingMode: constants.PacingEven, Status: constants.StatusLive,
			Targeting: targeting.Rules{
				Include: targeting.TargetingSet{Device: []string{"desktop"}},
			},
			Modifiers: targeting.Modifiers{
				GeoCountry: map[string]float64{"USA": 30, "GBR": 20},
			},
		},
	}
}

func pacingMode(s string) pacing.Mode {
	switch s {
	case "asap":
		return pacing.ModeASAP
	case "front_loaded":
		return pacing.ModeFrontLoaded
	default:
		return pacing.ModeEven
	}
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

// winHandler processes win notifications from the exchange.
// Decrements the campaign budget by the clearing price.
func winHandler(log *slog.Logger, budget *BudgetTracker, tracker *bidshading.Tracker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		bidID := q.Get("bid_id")
		price, _ := strconv.ParseFloat(q.Get("price"), 64)

		// Extract placement from bid ID if available
		placementID := q.Get("placement_id")
		campaignID := q.Get("campaign_id")

		// Record budget spend
		if campaignID != "" {
			budget.Record(campaignID, price)
		}

		// Record win for shading model
		if placementID != "" {
			tracker.RecordWin(placementID, price, price)
		}

		log.Info("win notification",
			"bid_id", bidID,
			"price", price,
			"campaign_id", campaignID,
		)
		w.WriteHeader(http.StatusNoContent)
	}
}

// lossHandler processes loss notifications from the exchange.
// Records loss data for the bid shading model.
func lossHandler(log *slog.Logger, tracker *bidshading.Tracker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		bidID := q.Get("bid_id")
		reason, _ := strconv.Atoi(q.Get("reason"))
		clearingPrice, _ := strconv.ParseFloat(q.Get("clearing_price"), 64)
		campaignID := q.Get("campaign_id")
		placementID := q.Get("placement_id")

		// Record loss for shading model
		if placementID != "" {
			tracker.RecordLoss(placementID, clearingPrice, clearingPrice, bidshading.LossReason(reason))
		}

		log.Info("loss notification",
			"bid_id", bidID,
			"reason", reason,
			"clearing_price", clearingPrice,
			"campaign_id", campaignID,
		)
		w.WriteHeader(http.StatusNoContent)
	}
}
