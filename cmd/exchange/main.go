// cmd/exchange is the Ad Exchange service.
// Receives bid requests, fans out to DSPs, runs auctions.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auction"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/fraud"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events/natsbus"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/health"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

func main() {
	clk := clock.Real{}
	log := logger.New(constants.ServiceExchange)
	sc := config.Setup(constants.ServiceExchange, log)
	cfg := sc.Cfg
	hlth := health.New()
	lc := lifecycle.New(log)

	port := cfg.Get("exchange.port", routes.PortExchange)
	channel := cfg.Get("exchange.channel", constants.ChannelAll)
	bidTimeout := cfg.GetDuration("exchange.bid_timeout", 100*time.Millisecond)
	dspEndpoints := strings.Split(cfg.Get("exchange.dsp_endpoints", routes.DefaultDSPURL+",http://localhost:"+routes.PortDSPComp1+",http://localhost:"+routes.PortDSPComp2), ",")

	engine := auction.NewEngine(clk)
	httpClient := &http.Client{Timeout: bidTimeout}

	// React to live config changes
	sc.Manager.OnChange("exchange.bid_timeout", func(_, _, newVal string) {
		if d, err := time.ParseDuration(newVal); err == nil {
			httpClient.Timeout = d
			log.Info("bid timeout updated live", "new", newVal)
		}
	})
	adsTxtCache := fraud.NewAdsTxtCache()

	// Connect to NATS for auction event publishing
	natsURL := cfg.Get("exchange.nats_url", routes.DefaultNATSURL)
	var pub *events.Publisher
	natsBus, err := natsbus.New(natsURL, constants.ServiceExchange, log)
	if err != nil {
		log.Warn("nats unavailable, auction events will not be published", "error", err)
	} else {
		ctx := context.Background()
		natsBus.EnsureStream(ctx, events.StreamName, []string{events.StreamSubjects})
		pub = events.NewPublisher(natsBus, log)
		lc.OnShutdown("nats", func(_ context.Context) error { return natsBus.Close() })
	}

	metrics := middleware.NewMetrics(constants.ServiceExchange)

	mux := http.NewServeMux()
	mux.Handle(routes.Healthz, hlth.LivenessHandler())
	mux.Handle(routes.Readyz, hlth.ReadinessHandler())
	mux.HandleFunc(routes.Metrics, metrics.Handler())

	mux.HandleFunc(routes.OpenRTBAuction, auctionHandler(log, clk, engine, httpClient, dspEndpoints, channel, pub, adsTxtCache))
	mux.HandleFunc(routes.OpenRTBWin, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc(routes.OpenRTBLoss, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })

	handler := metrics.Wrap(middleware.CORS(mux))

	server := &http.Server{Addr: ":" + port, Handler: handler, ReadTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second}

	log.Info("exchange starting", "port", port, "channel", channel, "dsps", dspEndpoints)
	lifecycle.ServeHTTP(lc, server, log, 30*time.Second)
}

func auctionHandler(log *slog.Logger, clk clock.Clock, engine *auction.Engine, client *http.Client, dspEndpoints []string, channel string, pub *events.Publisher, adsTxt *fraud.AdsTxtCache) http.HandlerFunc {
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

		traceID := bidReq.ID
		ctx := logger.WithTraceID(r.Context(), traceID)
		reqLog := logger.WithContext(log, ctx)
		start := clk.Now()

		reqLog.Info("auction started", "channel", channel, "num_dsps", len(dspEndpoints))

		// Fan out to DSPs in parallel
		bids, bidRecords := fanOutToDSPs(ctx, client, dspEndpoints, bidReq, reqLog)

		if len(bids) == 0 {
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
			json.NewEncoder(w).Encode(openrtb.BidResponse{ID: bidReq.ID, NoBid: true})
			reqLog.Info("auction complete", "result", "no_bids", "duration_ms", clk.Since(start).Milliseconds())
			return
		}

		// Run auction
		auctionReq := auction.AuctionRequest{
			RequestID:  bidReq.ID,
			Channel:    channel,
			PriceMode:  "first_price",
			FloorPrice: bidReq.Imp[0].BidFloor,
			TraceID:    traceID,
		}

		result, err := engine.RunAuction(ctx, bids, auctionReq)
		if err != nil {
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
			json.NewEncoder(w).Encode(openrtb.BidResponse{ID: bidReq.ID, NoBid: true})
			reqLog.Info("auction complete", "result", "no_winner", "error", err.Error(), "duration_ms", clk.Since(start).Milliseconds())
			return
		}

		winner := result.Winners[0]
		resp := openrtb.BidResponse{
			ID:  bidReq.ID,
			Cur: "USD",
			SeatBid: []openrtb.SeatBid{{
				Seat: winner.Bid.DSPID,
				Bid: []openrtb.BidObj{{
					ID:    fmt.Sprintf("win-%s", traceID),
					ImpID: bidReq.Imp[0].ID,
					Price: winner.ClearingPrice,
					CID:   winner.Bid.CampaignID,
					CrID:  winner.Bid.CreativeID,
				}},
			}},
		}

		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		json.NewEncoder(w).Encode(resp)

		reqLog.Info("auction complete",
			"result", "winner",
			"winner_dsp", winner.Bid.DSPID,
			"clearing_price", winner.ClearingPrice,
			"num_bids", len(bids),
			"duration_ms", clk.Since(start).Milliseconds(),
		)

		// Send win/loss notifications asynchronously
		go sendWinLossNotifications(client, bidRecords, winner.Bid.DSPID, winner.ClearingPrice, bidReq.Imp[0].BidFloor, reqLog)

		// Publish auction events to NATS
		if pub != nil {
			go func() {
				// AuctionWinEvent - single source of truth for cost
				pub.AuctionWin(ctx, events.AuctionWinEvent{
					TraceID:       traceID,
					AuctionID:     traceID,
					WinnerDSP:     winner.Bid.DSPID,
					CampaignID:    winner.Bid.CampaignID,
					CreativeID:    winner.Bid.CreativeID,
					PlacementID:   bidReq.Imp[0].ID,
					ClearingPrice: winner.ClearingPrice,
					Currency:      "USD",
					BidModel:      winner.Bid.BidModel,
					Channel:       channel,
					Timestamp:     clk.Now(),
				})

				// AuctionCompleteEvent - all bids for analytics
				var bidSummaries []events.BidSummary
				for _, b := range bids {
					bidSummaries = append(bidSummaries, events.BidSummary{
						DSPID:      b.DSPID,
						CampaignID: b.CampaignID,
						Price:      b.Price,
						Won:        b.DSPID == winner.Bid.DSPID,
					})
				}
				pub.AuctionComplete(ctx, events.AuctionCompleteEvent{
					TraceID:       traceID,
					PlacementID:   bidReq.Imp[0].ID,
					Channel:       channel,
					NumBids:       len(bids),
					WinnerDSP:     winner.Bid.DSPID,
					ClearingPrice: winner.ClearingPrice,
					FloorPrice:    bidReq.Imp[0].BidFloor,
					DurationMs:    clk.Since(start).Milliseconds(),
					Bids:          bidSummaries,
					Timestamp:     clk.Now(),
				})
			}()
		}
	}
}

// sendWinLossNotifications notifies each DSP whether they won or lost.
// Winner gets price confirmation. Losers get the reason and clearing price
// so they can adjust their bid shading models.
func sendWinLossNotifications(client *http.Client, records []dspBidRecord, winnerDSP string, clearingPrice, floorPrice float64, log *slog.Logger) {
	for _, rec := range records {
		if rec.Bid.DSPID == winnerDSP {
			// Win notification
			winURL := fmt.Sprintf("%s/v1/openrtb/win?bid_id=%s&price=%.4f",
				rec.Endpoint, rec.BidID, clearingPrice)
			resp, err := client.Get(winURL)
			if err != nil {
				log.Debug("win notification failed", "dsp", rec.Bid.DSPID, "error", err)
				continue
			}
			resp.Body.Close()
			log.Debug("win notification sent", "dsp", rec.Bid.DSPID, "price", clearingPrice)
		} else {
			// Loss notification with reason
			reason := 102 // outbid
			if rec.Bid.Price < floorPrice {
				reason = 100 // below floor
			}
			lossURL := fmt.Sprintf("%s/v1/openrtb/loss?bid_id=%s&reason=%d&clearing_price=%.4f&campaign_id=%s",
				rec.Endpoint, rec.BidID, reason, clearingPrice, rec.Bid.CampaignID)
			resp, err := client.Get(lossURL)
			if err != nil {
				log.Debug("loss notification failed", "dsp", rec.Bid.DSPID, "error", err)
				continue
			}
			resp.Body.Close()
			log.Debug("loss notification sent", "dsp", rec.Bid.DSPID, "reason", reason)
		}
	}
}

// dspBidRecord tracks which endpoint a bid came from so we can send win/loss notices.
type dspBidRecord struct {
	Bid      auction.Bid
	BidID    string // OpenRTB bid ID
	Endpoint string // DSP's base URL
}

func fanOutToDSPs(ctx context.Context, client *http.Client, endpoints []string, bidReq openrtb.BidRequest, log *slog.Logger) ([]auction.Bid, []dspBidRecord) {
	type dspResult struct {
		dspID   string
		bids    []auction.Bid
		records []dspBidRecord
		err     error
	}

	ch := make(chan dspResult, len(endpoints))

	for i, endpoint := range endpoints {
		dspID := fmt.Sprintf("dsp-%d", i)
		endpoint := strings.TrimSpace(endpoint)

		go func() {
			body, _ := json.Marshal(bidReq)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost,
				endpoint+routes.OpenRTBBid, bytes.NewReader(body))
			if err != nil {
				ch <- dspResult{dspID: dspID, err: err}
				return
			}
			req.Header.Set(constants.HeaderContentType, constants.ContentTypeJSON)

			start := time.Now()
			resp, err := client.Do(req)
			responseTime := time.Since(start)
			if err != nil {
				ch <- dspResult{dspID: dspID, err: err}
				return
			}
			defer resp.Body.Close()

			respBody, _ := io.ReadAll(resp.Body)
			var bidResp openrtb.BidResponse
			if err := json.Unmarshal(respBody, &bidResp); err != nil {
				ch <- dspResult{dspID: dspID, err: err}
				return
			}

			if bidResp.NoBid || len(bidResp.SeatBid) == 0 {
				ch <- dspResult{dspID: dspID}
				return
			}

			var bids []auction.Bid
			var records []dspBidRecord
			for _, sb := range bidResp.SeatBid {
				for _, b := range sb.Bid {
					bid := auction.Bid{
						DSPID:        dspID,
						CampaignID:   b.CID,
						CreativeID:   b.CrID,
						Price:        b.Price,
						Currency:     bidResp.Cur,
						BidModel:     "cpm",
						AdvertiserID: firstOrEmpty(b.ADomain),
						ResponseTime: responseTime,
					}
					bids = append(bids, bid)
					records = append(records, dspBidRecord{
						Bid:      bid,
						BidID:    b.ID,
						Endpoint: endpoint,
					})
				}
			}
			ch <- dspResult{dspID: dspID, bids: bids, records: records}
		}()
	}

	var allBids []auction.Bid
	var allRecords []dspBidRecord
	for range endpoints {
		result := <-ch
		if result.err != nil {
			log.Warn("dsp call failed", "dsp", result.dspID, "error", result.err)
			continue
		}
		allBids = append(allBids, result.bids...)
		allRecords = append(allRecords, result.records...)
	}
	return allBids, allRecords
}

func firstOrEmpty(s []string) string {
	if len(s) > 0 {
		return s[0]
	}
	return ""
}

