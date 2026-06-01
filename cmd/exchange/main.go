// cmd/exchange is the Ad Exchange service.
// Receives bid requests, fans out to DSPs, runs auctions.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auction"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache/warm"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/deals"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events/natsbus"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/fraud"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/health"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/models"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/optimise"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tracing"
	_ "github.com/lib/pq"
	"go.opentelemetry.io/otel/attribute"
)

func main() {
	clk := clock.Real{}
	log := logger.New(constants.ServiceExchange)
	sc := config.Setup(constants.ServiceExchange, log)
	cfg := sc.Cfg
	hlth := health.New()
	lc := lifecycle.New(log)

	// OpenTelemetry — exporter ships to Jaeger via OTLP/HTTP. Empty endpoint
	// disables tracing entirely so dev/test envs without Jaeger still boot.
	otelShutdown := tracing.Init(context.Background(), tracing.Config{
		ServiceName:    constants.ServiceExchange,
		ServiceVersion: cfg.Get("otel.service_version", "dev"),
		Endpoint:       cfg.Get("otel.endpoint", "localhost:4318"),
		SampleRatio:    cfg.GetFloat("otel.sample_ratio", 1.0),
		Log:            log,
	})
	lc.OnShutdown("otel", func(ctx context.Context) error { return otelShutdown(ctx) })

	config.PublishSchemaWithURL(cfg.Get("database.url", ""), constants.ServiceExchange, exchangeSchema, log)
	port := cfg.Get("exchange.port", routes.PortExchange)
	channel := cfg.Get("exchange.channel", constants.ChannelAll)
	bidTimeout := cfg.GetDuration("exchange.bid_timeout", 500*time.Millisecond)
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
	// Auction-domain counters (adtech_auctions_total, revenue, …) emitted
	// from the same /metrics endpoint as the generic HTTP metrics.
	auctionM := newAuctionMetrics(metrics.Registry())

	// Warm cache of active deals.
	dealCache := startDealCache(cfg, clk, log)
	if dealCache != nil {
		lc.OnShutdown("deal-cache", func(_ context.Context) error { dealCache.Stop(); return nil })
	}

	// Readiness: deal cache must have run at least once (an empty result
	// is still "ready" — empty is a valid state for fresh seed). Exchange
	// can run open auctions without deals, so the cache being available
	// is sufficient.
	hlth.AddReadinessCheck("deal-cache", func(_ context.Context) error {
		ts, err := dealCache.LastLoaded()
		if err != nil {
			return err
		}
		if ts.IsZero() {
			return errors.New("deal cache not yet loaded")
		}
		return nil
	})

	// Smart router: tracks per-DSP bid/win/timeout history; the auction
	// handler asks it to filter the fan-out list each request so we stop
	// calling DSPs that haven't bid in a long time.
	router := optimise.NewSmartRouter()
	routerMinCalls := cfg.GetInt("exchange.routing_min_calls", 20)
	_ = routerMinCalls // SmartRouter uses 20 as a hardcoded threshold today

	mux := http.NewServeMux()
	mux.Handle(routes.Healthz, hlth.LivenessHandler())
	mux.Handle(routes.Readyz, hlth.ReadinessHandler())
	mux.Handle(routes.Metrics, metrics.Handler())

	// Debug: list deals currently in the warm cache
	mux.HandleFunc("/v1/openrtb/deals", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		json.NewEncoder(w).Encode(dealCache.All())
	})

	if cfg.GetBool("debug.endpoints_enabled", true) {
		mux.HandleFunc(routes.DebugCacheRefresh, warm.RefreshHandler(dealCache))
	}

	// Debug: per-DSP routing stats so we can see what the smart router learned
	mux.HandleFunc("/v1/openrtb/routing", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		json.NewEncoder(w).Encode(router.Stats())
	})

	// Getter for the live bid_timeout config so the auction handler reads
	// the current value per request (live config changes apply on next auction
	// without restarts). Also used as the fan-out context deadline so the
	// gather loop can early-finish via ctx.Done() once it elapses.
	bidTimeoutFn := func() time.Duration {
		return cfg.GetDuration("exchange.bid_timeout", 500*time.Millisecond)
	}
	mux.HandleFunc(routes.OpenRTBAuction, auctionHandler(log, clk, engine, httpClient, bidTimeoutFn, dspEndpoints, channel, pub, adsTxtCache, dealCache, router, auctionM))
	mux.HandleFunc(routes.OpenRTBWin, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc(routes.OpenRTBLoss, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })

	handler := tracing.HTTPMiddleware(constants.ServiceExchange)(metrics.Wrap(middleware.CORS(mux)))

	server := &http.Server{Addr: ":" + port, Handler: handler, ReadTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second}

	log.Info("exchange starting", "port", port, "channel", channel, "dsps", dspEndpoints)
	lifecycle.ServeHTTP(lc, server, log, 30*time.Second)
}

func startDealCache(cfg *config.Config, clk clock.Clock, log *slog.Logger) *warm.Cache[models.Deal] {
	pollInterval := firstNonZeroDuration(
		cfg.GetDuration("cache.warm.deals.poll_interval", 0),
		cfg.GetDuration("cache.warm.poll_interval", 30*time.Second),
	)
	loader := pickDealLoader(cfg, log)
	bus := connectInvalidateBus(cfg, log)
	c := warm.New(warm.Config[models.Deal]{
		Name:              "deals",
		Loader:            loader,
		Clock:             clk,
		Bus:               bus,
		InvalidateSubject: events.SubjectCacheInvalidateDeals,
		PollInterval:      pollInterval,
		Log:               log,
	})
	if err := c.Start(context.Background()); err != nil {
		log.Warn("deal cache initial load failed (continuing with empty set)", "error", err)
	}
	return c
}

func pickDealLoader(cfg *config.Config, log *slog.Logger) warm.Loader[models.Deal] {
	dbURL := cfg.Get("database.url", "")
	if dbURL == "" {
		log.Warn("database.url not set, deal cache will be empty")
		return emptyDealLoader{}
	}
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Warn("postgres open failed", "error", err)
		return emptyDealLoader{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		log.Warn("postgres ping failed", "error", err)
		_ = db.Close()
		return emptyDealLoader{}
	}
	store, _ := postgres.New(postgres.Config{PrimaryURL: dbURL, MaxOpenConns: 5, MaxIdleConns: 2, ConnMaxLifetime: 5 * time.Minute})
	log.Info("postgres connected for deal loader")
	return &postgres.DealLoader{Store: store}
}

type emptyDealLoader struct{}

func (emptyDealLoader) LoadAll(_ context.Context) ([]models.Deal, error) { return nil, nil }
func (emptyDealLoader) KeyOf(d models.Deal) string                       { return d.ID }

// connectInvalidateBus returns a NATS bus used only for cache invalidates.
// The auction publisher uses its own bus instance already; this one stays
// scoped to the warm cache so its lifecycle is independent.
func connectInvalidateBus(cfg *config.Config, log *slog.Logger) events.EventBus {
	url := cfg.Get("exchange.nats_url", routes.DefaultNATSURL)
	bus, err := natsbus.New(url, constants.ServiceExchange+"-cache", log)
	if err != nil {
		log.Warn("nats unavailable for cache invalidate, polling only", "error", err)
		return nil
	}
	return bus
}

func firstNonZeroDuration(ds ...time.Duration) time.Duration {
	for _, d := range ds {
		if d > 0 {
			return d
		}
	}
	return 30 * time.Second
}

func auctionHandler(log *slog.Logger, clk clock.Clock, engine *auction.Engine, client *http.Client, bidTimeoutFn func() time.Duration, dspEndpoints []string, channel string, pub *events.Publisher, adsTxt *fraud.AdsTxtCache, dealCache *warm.Cache[models.Deal], router *optimise.SmartRouter, am *auctionMetrics) http.HandlerFunc {
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

		// W3C trace ID from the OTel span HTTPMiddleware created (or
		// extracted from the inbound traceparent header if the SSP propagated
		// one). Falls back to bidReq.ID for callers that still send their
		// own opaque ID (legacy simulator, e2e harness). Same string flows
		// into logs, NATS events, analytics, billing — single ID end-to-end.
		traceID := tracing.TraceIDFromContext(r.Context())
		if traceID == "" {
			traceID = bidReq.ID
		}
		ctx := logger.WithTraceID(r.Context(), traceID)
		reqLog := logger.WithContext(log, ctx)
		start := clk.Now()

		// Application-level span — the HTTP middleware already opened a
		// server span around the request, but we want the auction phases
		// (fanout, deal-eval, win-loss) to show as named children in Jaeger.
		ctx, auctionSpan := tracing.StartSpan(ctx, "exchange.auction",
			attribute.String("auction.trace_id", traceID),
			attribute.String("auction.channel", channel),
		)
		defer auctionSpan.End()

		// Smart routing: filter the DSP list to the ones likely to bid.
		// First-time/unseen DSPs get a neutral score and stay in. Heavy no-bid
		// or timeout patterns drop a DSP for this auction (still gets occasional
		// traffic via the periodic poll model — see optimise.SmartRouter).
		selectedEndpoints := router.SelectDSPs(channel, dspEndpoints)
		if len(selectedEndpoints) == 0 {
			// Safety floor: if the router would skip everyone (cold start edge
			// case or learned-bad state), fall back to the full list. We never
			// want to silently no-bid because of routing.
			selectedEndpoints = dspEndpoints
		}

		reqLog.Info("auction started", "channel", channel, "num_dsps_total", len(dspEndpoints), "num_dsps_called", len(selectedEndpoints))

		// Fan out to DSPs in parallel. The fan-out context carries the
		// bid_timeout deadline so:
		//   1. Slow DSPs whose HTTP call would exceed bid_timeout get cancelled
		//      via context propagation (client.Timeout is the belt-and-braces
		//      backstop for the same).
		//   2. The gather loop in fanOutToDSPs uses ctx.Done() to early-finish
		//      once the deadline elapses — auctions complete on the configured
		//      bid_timeout, not on "slowest DSP's actual response time."
		bidTimeout := bidTimeoutFn()
		fanCtx, fanCancel := context.WithTimeout(ctx, bidTimeout)
		defer fanCancel()
		fanCtx, fanSpan := tracing.StartSpan(fanCtx, "exchange.fanout",
			attribute.Int("dsps.called", len(selectedEndpoints)),
			attribute.Int64("bid_timeout_ms", bidTimeout.Milliseconds()),
		)
		// Dev-mode: publisher simulator can send a CSV of DSP indexes that
		// should be deliberately slow this auction, so the UI can demo the
		// timeout / early-finish behavior. Header is opt-in per request; an
		// empty value means "all DSPs run normally."
		slowDSPs := parseSlowDSPs(r.Header.Get("X-Dev-Slow-DSPs"))

		bids, bidRecords := fanOutToDSPs(fanCtx, client, selectedEndpoints, bidReq, channel, slowDSPs, reqLog, router)
		fanSpan.SetAttributes(attribute.Int("bids.received", len(bids)))
		fanSpan.End()

		if len(bids) == 0 {
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
			json.NewEncoder(w).Encode(openrtb.BidResponse{ID: bidReq.ID, NoBid: true})
			reqLog.Info("auction complete", "result", "no_bids", "duration_ms", clk.Since(start).Milliseconds())
			am.auctionsTotal.WithLabelValues("no_bids", channel).Inc()
			return
		}

		// Deal evaluation: per-bid decision (deal_id, effective floor, preempt).
		// Bids below their per-bid effective floor are dropped. A PG bid short-
		// circuits the auction — that bid wins at the deal price.
		placementID := ""
		publisherID := ""
		if bidReq.Site != nil && bidReq.Site.Publisher != nil && bidReq.Site.Publisher.ID != "" {
			// SSP populates Site.Publisher.ID with the publisher UUID; deal
			// matching uses UUIDs so this is the right key. Fall back to
			// domain when the SSP didn't set it (legacy callers / tests).
			publisherID = bidReq.Site.Publisher.ID
		} else if bidReq.Site != nil {
			publisherID = bidReq.Site.Domain
		}
		if len(bidReq.Imp) > 0 {
			// Imp.TagID carries the placement UUID (set by SSP); fall back
			// to Imp.ID for legacy callers that haven't been updated.
			if bidReq.Imp[0].TagID != "" {
				placementID = bidReq.Imp[0].TagID
			} else {
				placementID = bidReq.Imp[0].ID
			}
		}
		matcher := deals.New(dealCache.All())
		var eligibleBids []auction.Bid
		var preemptBid *auction.Bid
		var preemptPrice float64
		var preemptDealID string
		for i := range bids {
			b := bids[i]
			matches := matcher.Match(deals.Request{
				PublisherID:  publisherID,
				PlacementID:  placementID,
				AdvertiserID: b.AdvertiserID,
				Now:          clk.Now(),
			})
			dec := deals.Decide(b.AdvertiserID, bidReq.Imp[0].BidFloor, matches)
			if dec.Preempt {
				preemptBid = &b
				preemptPrice = dec.EffectiveFloor
				preemptDealID = dec.DealID
				break
			}
			if b.Price < dec.EffectiveFloor {
				reqLog.Debug("bid below effective floor", "bid_dsp", b.DSPID, "price", b.Price, "floor", dec.EffectiveFloor, "deal", dec.DealID)
				am.bidsBelowFloorTotal.WithLabelValues(placementID).Inc()
				continue
			}
			b.DealID = dec.DealID
			eligibleBids = append(eligibleBids, b)
		}

		var (
			winnerBid     auction.Bid
			clearingPrice float64
			winningDealID string
		)
		if preemptBid != nil {
			winnerBid = *preemptBid
			clearingPrice = preemptPrice
			winningDealID = preemptDealID
			reqLog.Info("auction preempted by PG deal", "deal_id", preemptDealID, "winner_dsp", preemptBid.DSPID, "price", preemptPrice)
		} else {
			if len(eligibleBids) == 0 {
				w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
				json.NewEncoder(w).Encode(openrtb.BidResponse{ID: bidReq.ID, NoBid: true})
				reqLog.Info("auction complete", "result", "all_bids_below_floor", "num_bids", len(bids), "duration_ms", clk.Since(start).Milliseconds())
				am.auctionsTotal.WithLabelValues("all_below_floor", channel).Inc()
				return
			}
			auctionReq := auction.AuctionRequest{
				RequestID:  bidReq.ID,
				Channel:    channel,
				PriceMode:  "first_price",
				FloorPrice: bidReq.Imp[0].BidFloor,
				TraceID:    traceID,
			}
			result, err := engine.RunAuction(ctx, eligibleBids, auctionReq)
			if err != nil {
				w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
				json.NewEncoder(w).Encode(openrtb.BidResponse{ID: bidReq.ID, NoBid: true})
				reqLog.Info("auction complete", "result", "no_winner", "error", err.Error(), "duration_ms", clk.Since(start).Milliseconds())
				am.auctionsTotal.WithLabelValues("no_winner", channel).Inc()
				return
			}
			winner := result.Winners[0]
			winnerBid = winner.Bid
			clearingPrice = winner.ClearingPrice
			winningDealID = winner.Bid.DealID
		}

		// Seat = the winning advertiser's UUID (sb.Seat from DSP). Falls back
		// to the DSP node id when seat wasn't set. Without this, the SSP/test
		// harness reads winner.Seat as "dsp-1" instead of the actual
		// advertiser UUID and assertions like "expected seat == advAccID" fail.
		winnerSeat := winnerBid.AdvertiserID
		if winnerSeat == "" {
			winnerSeat = winnerBid.DSPID
		}
		resp := openrtb.BidResponse{
			ID:  bidReq.ID,
			Cur: "USD",
			SeatBid: []openrtb.SeatBid{{
				Seat: winnerSeat,
				Bid: []openrtb.BidObj{{
					ID:    fmt.Sprintf("win-%s", traceID),
					ImpID: bidReq.Imp[0].ID,
					Price: clearingPrice,
					CID:   winnerBid.CampaignID,
					CrID:  winnerBid.CreativeID,
					// Deal ID flows into the response so SSPs/tools see which
					// deal the auction cleared under. The auction handler
					// computed this above (winningDealID); reusing here keeps
					// the response consistent with NATS AuctionWinEvent.DealID.
					DealID: winningDealID,
				}},
			}},
		}

		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		json.NewEncoder(w).Encode(resp)

		reqLog.Info("auction complete",
			"result", "winner",
			"winner_dsp", winnerBid.DSPID,
			"clearing_price", clearingPrice,
			"deal_id", winningDealID,
			"num_bids", len(bids),
			"eligible_bids", len(eligibleBids),
			"duration_ms", clk.Since(start).Milliseconds(),
		)
		// Domain counters for the Pipeline Health dashboard.
		am.auctionsTotal.WithLabelValues("winner", channel).Inc()
		am.clearingPriceUSDTotal.Add(clearingPrice)

		// Tag the auction span with the outcome so dev tools (pub sim, trace
		// explorer) can show winner + clearing price without an extra query.
		// These attributes are dev/ops surface only — the OpenRTB response
		// going back to the SSP/browser deliberately omits them.
		var winnerEndpoint string
		for _, rec := range bidRecords {
			if rec.Bid.DSPID == winnerBid.DSPID {
				winnerEndpoint = rec.Endpoint
				break
			}
		}
		auctionSpan.SetAttributes(
			attribute.String("auction.winner_dsp", winnerBid.DSPID),
			attribute.String("auction.winner_endpoint", winnerEndpoint),
			attribute.Float64("auction.clearing_price", clearingPrice),
			attribute.Int("auction.num_bids", len(bids)),
			attribute.Int("auction.eligible_bids", len(eligibleBids)),
			attribute.String("auction.deal_id", winningDealID),
		)

		// Record the win for the smart router using the winner's endpoint URL
		// (looked up from the bid records). This feeds back into SelectDSPs
		// for future auctions.
		for _, rec := range bidRecords {
			if rec.Bid.DSPID == winnerBid.DSPID {
				router.RecordWin(channel, rec.Endpoint)
				break
			}
		}

		// Send win/loss notifications asynchronously. Pass WithoutCancel(ctx)
		// so the trace context survives the handler returning (without it the
		// goroutine would lose access to the active span and the notify HTTP
		// calls would appear as detached traces in Jaeger).
		go sendWinLossNotifications(context.WithoutCancel(ctx), client, bidRecords, winnerBid.DSPID, clearingPrice, bidReq.Imp[0].BidFloor, placementID, reqLog)

		// Publish auction events to NATS
		if pub != nil {
			// Detach from the request context: the HTTP handler returns as
			// soon as the response is written, which would cancel ctx and
			// abort the publish mid-flight. WithoutCancel preserves the
			// trace_id value but isolates lifecycle.
			pubCtx := context.WithoutCancel(ctx)
			go func() {
				// AuctionWinEvent - single source of truth for cost
				pub.AuctionWin(pubCtx, events.AuctionWinEvent{
					TraceID:       traceID,
					AuctionID:     traceID,
					WinnerDSP:     winnerBid.DSPID,
					CampaignID:    winnerBid.CampaignID,
					CreativeID:    winnerBid.CreativeID,
					PlacementID:   bidReq.Imp[0].ID,
					ClearingPrice: clearingPrice,
					Currency:      "USD",
					BidModel:      winnerBid.BidModel,
					Channel:       channel,
					DealID:        winningDealID,
					Timestamp:     clk.Now(),
				})

				// AuctionCompleteEvent - all bids for analytics
				var bidSummaries []events.BidSummary
				for _, b := range bids {
					bidSummaries = append(bidSummaries, events.BidSummary{
						DSPID:      b.DSPID,
						CampaignID: b.CampaignID,
						Price:      b.Price,
						Won:        b.DSPID == winnerBid.DSPID,
					})
				}
				pub.AuctionComplete(pubCtx, events.AuctionCompleteEvent{
					TraceID:       traceID,
					PlacementID:   bidReq.Imp[0].ID,
					Channel:       channel,
					NumBids:       len(bids),
					WinnerDSP:     winnerBid.DSPID,
					ClearingPrice: clearingPrice,
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
//
// Trace propagation: each outbound HTTP call uses NewRequestWithContext +
// tracing.InjectHTTP so the DSP-side server span becomes a child of this
// fan-out (which is itself a child of the auction). Without InjectHTTP the
// DSP would start a fresh trace_id on the inbound and the win/loss spans
// would appear as detached traces in Jaeger instead of under the auction.
func sendWinLossNotifications(ctx context.Context, client *http.Client, records []dspBidRecord, winnerDSP string, clearingPrice, floorPrice float64, placementID string, log *slog.Logger) {
	ctx, span := tracing.StartSpan(ctx, "exchange.winloss_notify",
		attribute.Int("notify.count", len(records)),
		attribute.String("notify.winner_dsp", winnerDSP),
	)
	defer span.End()

	for _, rec := range records {
		isWin := rec.Bid.DSPID == winnerDSP
		var url string
		if isWin {
			// Win notification — campaign_id is required so the DSP can
			// decrement the right budget counter (Redis IncrBy keyed on
			// campaign_id). Without it, budget caps never trigger.
			url = fmt.Sprintf("%s/v1/openrtb/win?bid_id=%s&price=%.4f&campaign_id=%s&placement_id=%s",
				rec.Endpoint, rec.BidID, clearingPrice, rec.Bid.CampaignID, placementID)
		} else {
			// Loss notification with reason
			reason := 102 // outbid
			if rec.Bid.Price < floorPrice {
				reason = 100 // below floor
			}
			url = fmt.Sprintf("%s/v1/openrtb/loss?bid_id=%s&reason=%d&clearing_price=%.4f&campaign_id=%s&placement_id=%s",
				rec.Endpoint, rec.BidID, reason, clearingPrice, rec.Bid.CampaignID, placementID)
		}
		sendNotify(ctx, client, url, isWin, rec, log)
	}
}

// sendNotify is one win-or-loss notification with its own child span so each
// DSP call shows independently in Jaeger.
func sendNotify(ctx context.Context, client *http.Client, url string, isWin bool, rec dspBidRecord, log *slog.Logger) {
	kind := "loss"
	if isWin {
		kind = "win"
	}
	ctx, span := tracing.StartSpan(ctx, "exchange.notify."+kind,
		attribute.String("notify.dsp", rec.Bid.DSPID),
		attribute.String("notify.endpoint", rec.Endpoint),
	)
	defer span.End()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		span.RecordError(err)
		log.Debug(kind+" notification: build request failed", "dsp", rec.Bid.DSPID, "error", err)
		return
	}
	tracing.InjectHTTP(ctx, req)
	resp, err := client.Do(req)
	if err != nil {
		span.RecordError(err)
		log.Debug(kind+" notification failed", "dsp", rec.Bid.DSPID, "error", err)
		return
	}
	resp.Body.Close()
	span.SetAttributes(attribute.Int("http.status_code", resp.StatusCode))
	log.Debug(kind+" notification sent", "dsp", rec.Bid.DSPID)
}

// dspBidRecord tracks which endpoint a bid came from so we can send win/loss notices.
type dspBidRecord struct {
	Bid      auction.Bid
	BidID    string // OpenRTB bid ID
	Endpoint string // DSP's base URL
}

// parseSlowDSPs reads a CSV of DSP indexes (e.g. "0,2") from the inbound
// X-Dev-Slow-DSPs header and returns them as a set for O(1) lookup. Used
// only by the publisher simulator to demonstrate slow-DSP behavior; an
// unauthenticated header on a real bid request would let any caller add
// latency, so this should be gated by debug.endpoints_enabled in callers
// — currently the dev/staging deployment runs with that on and prod will
// not, so prod requests pass empty here and the header is ignored.
func parseSlowDSPs(csv string) map[int]bool {
	if csv == "" {
		return nil
	}
	out := map[int]bool{}
	for _, s := range strings.Split(csv, ",") {
		if i, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
			out[i] = true
		}
	}
	return out
}

func fanOutToDSPs(ctx context.Context, client *http.Client, endpoints []string, bidReq openrtb.BidRequest, channel string, slowDSPs map[int]bool, log *slog.Logger, router *optimise.SmartRouter) ([]auction.Bid, []dspBidRecord) {
	type dspResult struct {
		dspID    string
		endpoint string
		bids     []auction.Bid
		records  []dspBidRecord
		err      error
		latency  time.Duration
		timedOut bool
		topBid   float64
	}

	ch := make(chan dspResult, len(endpoints))

	for i, endpoint := range endpoints {
		dspID := fmt.Sprintf("dsp-%d", i)
		endpoint := strings.TrimSpace(endpoint)
		i := i // capture for goroutine

		go func() {
			body, _ := json.Marshal(bidReq)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost,
				endpoint+routes.OpenRTBBid, bytes.NewReader(body))
			if err != nil {
				ch <- dspResult{dspID: dspID, endpoint: endpoint, err: err}
				return
			}
			req.Header.Set(constants.HeaderContentType, constants.ContentTypeJSON)
			// Inject the W3C trace context so the DSP-side server span
			// becomes a child of this fanout span in Jaeger.
			tracing.InjectHTTP(ctx, req)
			// Dev-mode: if the inbound auction request flagged this DSP
			// index as "slow," tell the DSP to add artificial latency.
			// DSP gates the header on debug.endpoints_enabled.
			if slowDSPs[i] {
				req.Header.Set("X-Dev-Delay-Ms", "150")
			}

			start := time.Now()
			resp, err := client.Do(req)
			responseTime := time.Since(start)
			if err != nil {
				// Distinguish timeouts from other failures so the router
				// can penalise high-timeout DSPs specifically.
				timedOut := ctx.Err() != nil || strings.Contains(err.Error(), "deadline")
				ch <- dspResult{dspID: dspID, endpoint: endpoint, err: err, latency: responseTime, timedOut: timedOut}
				return
			}
			defer resp.Body.Close()

			respBody, _ := io.ReadAll(resp.Body)
			var bidResp openrtb.BidResponse
			if err := json.Unmarshal(respBody, &bidResp); err != nil {
				ch <- dspResult{dspID: dspID, endpoint: endpoint, err: err, latency: responseTime}
				return
			}

			if bidResp.NoBid || len(bidResp.SeatBid) == 0 {
				ch <- dspResult{dspID: dspID, endpoint: endpoint, latency: responseTime}
				return
			}

			var bids []auction.Bid
			var records []dspBidRecord
			var topBid float64
			for _, sb := range bidResp.SeatBid {
				for _, b := range sb.Bid {
					// Seat is the advertiser/account UUID set by the DSP. We
					// use it for deal allowlist matching; ADomain is the creative
					// landing domain and used elsewhere for safety/blocklists.
					bid := auction.Bid{
						DSPID:        dspID,
						CampaignID:   b.CID,
						CreativeID:   b.CrID,
						Price:        b.Price,
						Currency:     bidResp.Cur,
						BidModel:     "cpm",
						AdvertiserID: sb.Seat,
						ResponseTime: responseTime,
					}
					bids = append(bids, bid)
					records = append(records, dspBidRecord{
						Bid:      bid,
						BidID:    b.ID,
						Endpoint: endpoint,
					})
					if b.Price > topBid {
						topBid = b.Price
					}
				}
			}
			ch <- dspResult{dspID: dspID, endpoint: endpoint, bids: bids, records: records, latency: responseTime, topBid: topBid}
		}()
	}

	var allBids []auction.Bid
	var allRecords []dspBidRecord
	received := 0
	// Early-finish: once the fan-out context deadline elapses, stop waiting
	// for in-flight DSP goroutines. They'll be cancelled via ctx propagation
	// and exit on their own (their channel sends become wasted writes into a
	// buffered channel that nobody reads, which is fine — the channel has
	// capacity = len(endpoints)). Without this, a single slow DSP whose
	// HTTP call hasn't yet returned (despite ctx cancel) would force the
	// auction to wait for it just to read its "timed out" result.
	for received < len(endpoints) {
		select {
		case result := <-ch:
			// Endpoint URL is the stable key for routing stats (DSP indexes
			// can shift if config changes, URLs don't).
			bidReceived := len(result.bids) > 0
			router.RecordCall(channel, result.endpoint, bidReceived, result.topBid, result.latency, result.timedOut)
			if result.err != nil {
				log.Warn("dsp call failed", "dsp", result.dspID, "endpoint", result.endpoint, "error", result.err)
			} else {
				allBids = append(allBids, result.bids...)
				allRecords = append(allRecords, result.records...)
			}
			received++
		case <-ctx.Done():
			log.Debug("fan-out deadline elapsed before all DSPs reported",
				"received", received, "total", len(endpoints))
			return allBids, allRecords
		}
	}
	return allBids, allRecords
}



// rebuild trigger
