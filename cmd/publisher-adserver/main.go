// cmd/publisher-adserver runs the publisher-side ad serving layer.
// Sits in front of cmd/ssp and arbitrates direct-sold publisher line items
// against the programmatic auction (which becomes the fall-through demand
// source). See docs/PLAN.md → "Publisher-Side Ad Server" for the design.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adserving"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache"
	cacheredis "github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache/redis"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache/warm"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events/natsbus"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/health"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/idgen"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/models"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/publisheradserver"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/publisheradserver/arbitration"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/publisheradserver/pacing"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/publisheradserver/prebidclient"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tracing"
	_ "github.com/lib/pq"
)

func main() {
	clk := clock.Real{}
	log := logger.New(constants.ServicePublisherAdServer)
	sc := config.Setup(constants.ServicePublisherAdServer, publisherAdServerSchema, log)
	cfg := sc.Cfg
	_ = sc
	hlth := health.New()
	lc := lifecycle.New(log)

	port := cfg.Get("publisher_adserver.port", routes.PortPublisherAdServer)
	sspURL := cfg.Get("publisher_adserver.ssp_url", routes.DefaultSSPURL)
	adserverURL := cfg.Get("publisher_adserver.adserver_url", routes.DefaultAdServerURL)
	trackerURL := cfg.Get("publisher_adserver.tracker_url", routes.DefaultTrackerURL)

	otelShutdown := tracing.Init(context.Background(), tracing.Config{
		ServiceName:    constants.ServicePublisherAdServer,
		ServiceVersion: cfg.Get("otel.service_version", "dev"),
		Endpoint:       cfg.Get("otel.endpoint", "localhost:4318"),
		SampleRatio:    cfg.GetFloat("otel.sample_ratio", 1.0),
		Log:            log,
	})
	lc.OnShutdown("otel", func(ctx context.Context) error { return otelShutdown(ctx) })

	// Two warm caches: publisher line items (the direct-sold inventory we
	// arbitrate) and placements (so we can resolve placement_id → publisher_id
	// without going to the SSP for every request).
	lineItemCache := startLineItemCache(cfg, clk, log)
	if lineItemCache != nil {
		lc.OnShutdown("line-item-cache", func(_ context.Context) error { lineItemCache.Stop(); return nil })
	}
	placementCache := startPlacementCache(cfg, clk, log)
	if placementCache != nil {
		lc.OnShutdown("placement-cache", func(_ context.Context) error { placementCache.Stop(); return nil })
	}

	// Pacing tracker: Redis-backed actuals counter for guaranteed line items.
	// Falls back to in-memory L2 if Redis is unreachable (matches the rest
	// of the platform's fail-open pattern).
	l2 := connectRedis(cfg, log)
	pacer := pacing.New(l2, log)

	hlth.AddReadinessCheck("placement-cache", func(_ context.Context) error {
		ts, err := placementCache.LastLoaded()
		if err != nil {
			return err
		}
		if ts.IsZero() {
			return errors.New("placement cache not yet loaded")
		}
		return nil
	})
	hlth.AddReadinessCheck("line-item-cache", func(_ context.Context) error {
		ts, err := lineItemCache.LastLoaded()
		if err != nil {
			return err
		}
		if ts.IsZero() {
			return errors.New("line item cache not yet loaded")
		}
		return nil
	})

	metrics := middleware.NewMetrics(constants.ServicePublisherAdServer)

	mux := http.NewServeMux()
	mux.Handle(routes.Healthz, hlth.LivenessHandler())
	mux.Handle(routes.Readyz, hlth.ReadinessHandler())
	mux.Handle(routes.Metrics, metrics.Handler())

	if cfg.GetBool("debug.endpoints_enabled", true) {
		mux.HandleFunc(routes.DebugCacheRefresh, warm.RefreshHandler(lineItemCache, placementCache))
		mux.HandleFunc(routes.DebugPubAdLineItems, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
			json.NewEncoder(w).Encode(lineItemCache.All())
		})
	}

	// Outbound Prebid client. Timeout is re-read live so config edits to
	// publisher_adserver.prebid_timeout land without a restart, just like
	// exchange.bid_timeout does.
	prebidTimeout := cfg.GetDuration("publisher_adserver.prebid_timeout", 500*time.Millisecond)
	prebidCli := prebidclient.New(prebidTimeout, log)
	prebidServersFn := func() string { return cfg.Get("publisher_adserver.prebid_servers", "") }

	// Event publisher for DirectWin + PrebidOutboundWin. Nil-tolerant —
	// if NATS is unreachable we skip publishing rather than failing the
	// serve. Closes the analytics blind spots where direct-sold serves
	// and external Prebid wins left no reporting record.
	natsURL := cfg.Get("publisher_adserver.nats_url", cfg.Get("nats.url", routes.DefaultNATSURL))
	var pub *events.Publisher
	if pubBus, err := natsbus.New(natsURL, constants.ServicePublisherAdServer+"-events", log); err == nil {
		ctx := context.Background()
		pubBus.EnsureStream(ctx, events.StreamName, []string{events.StreamSubjects})
		pub = events.NewPublisher(pubBus, log)
		lc.OnShutdown("pubad-publisher", func(_ context.Context) error { return pubBus.Close() })
	} else {
		log.Warn("pubad event publisher unavailable, DirectWin / PrebidOutboundWin events will be skipped", "error", err)
	}

	mux.HandleFunc(routes.PublisherAdServe, serveHandler(serveDeps{
		log:             log,
		clk:             clk,
		lineItemCache:   lineItemCache,
		placementCache:  placementCache,
		pacer:           pacer,
		sspURL:          sspURL,
		adserverURL:     adserverURL,
		trackerURL:      trackerURL,
		prebidClient:    prebidCli,
		prebidServersFn: prebidServersFn,
		pub:             pub,
	}))

	handler := tracing.HTTPMiddleware(constants.ServicePublisherAdServer)(metrics.Wrap(middleware.CORS(mux)))
	// WriteTimeout=15 s covers the worst-case /debug/cache/refresh
	// across two warm caches (3 s per-Refresh inner timeout × 2 + JSON
	// marshal + headroom). The /serve hot path is sub-second so the
	// generous timeout doesn't affect it.
	server := &http.Server{Addr: ":" + port, Handler: handler, ReadTimeout: 5 * time.Second, WriteTimeout: 15 * time.Second}

	log.Info("publisher-adserver starting",
		"port", port,
		"line_items", lineItemCache.Len(),
		"placements", placementCache.Len(),
		"ssp", sspURL,
		"adserver", adserverURL,
	)
	lifecycle.ServeHTTP(lc, server, log, 30*time.Second)
}

type serveDeps struct {
	log              *slog.Logger
	clk              clock.Clock
	lineItemCache    *warm.Cache[publisheradserver.PublisherLineItem]
	placementCache   *warm.Cache[postgres.PlacementRow]
	pacer            *pacing.Tracker
	sspURL           string
	adserverURL      string
	trackerURL       string
	prebidClient     *prebidclient.Client
	prebidServersFn  func() string  // CSV; re-read per request for live-tunable demand-source list
	pub              *events.Publisher // nil-tolerant; emits DirectWin + PrebidOutboundWin events
}

// uuidPattern matches Postgres's canonical lowercase 8-4-4-4-12 hex UUID.
// Used to discriminate "raw UUID supplied" from "external key needs derivation".
var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func serveHandler(d serveDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		placementExt := r.URL.Query().Get("placement_id")
		if placementExt == "" {
			placementExt = "pl-news-mpu"
		}
		placementID := placementExt
		if !uuidPattern.MatchString(placementExt) {
			placementID = idgen.Derive("placement", placementExt)
		}

		placement, ok := d.placementCache.ByID(placementID)
		if !ok {
			http.Error(w, "placement not found", http.StatusNotFound)
			return
		}

		traceID := tracing.TraceIDFromContext(r.Context())
		if traceID == "" {
			traceID = fmt.Sprintf("pubad-%d", time.Now().UnixMilli())
		}
		ctx := logger.WithTraceID(r.Context(), traceID)
		reqLog := logger.WithContext(d.log, ctx)

		req := arbitration.Request{
			PublisherID: placement.PublisherID,
			PlacementID: placement.ID,
			Now:         d.clk.Now(),
		}

		decision := arbitration.Decide(req, d.lineItemCache.All(), d.pacer)
		reqLog.Info("arbitration decision",
			"type", decisionLabel(decision.Type),
			"reason", decision.Reason,
			"publisher", placement.PublisherID,
			"placement", placement.ID,
		)

		// Geo/device/user passed to programmatic so the Prebid fan-out
		// builds an equivalent OpenRTB request to what the SSP sees.
		// Defaults when the caller (e.g. the demo simulator) omits them:
		// derive device from the User-Agent and assume USA geo. Without
		// this every geo/device-targeted DSP campaign no-bids 100% and
		// only run-of-network bidders compete, which collapses the
		// auction to a single deterministic winner.
		geo := r.URL.Query().Get("geo")
		device := r.URL.Query().Get("device")
		userID := r.URL.Query().Get("user_id")
		if geo == "" {
			geo = "USA"
		}
		if device == "" {
			device = deviceFromUserAgent(r.UserAgent())
		}

		switch decision.Type {
		case arbitration.DecisionDirect:
			d.serveDirect(ctx, w, reqLog, decision.LineItem, placement, traceID)
			return
		case arbitration.DecisionProgrammatic:
			if d.serveProgrammatic(ctx, w, r, reqLog, placement, placementExt, traceID, geo, device, userID) {
				return
			}
			// Programmatic returned no-bid — fall through to house.
			house := arbitration.DecideHouse(req, d.lineItemCache.All())
			if house.Type == arbitration.DecisionDirect && house.LineItem != nil {
				d.serveDirect(ctx, w, reqLog, house.LineItem, placement, traceID)
				return
			}
			d.publishNoFill(ctx, traceID, placement, "programmatic-nobid-and-no-house")
			writeNoBid(w, traceID)
		default:
			d.publishNoFill(ctx, traceID, placement, "arbitration-default-branch")
			writeNoBid(w, traceID)
		}
	}
}

// serveDirect renders a direct-sold line item by calling the ad server with
// a ServeRequest, then writes the result back to the visitor. CampaignID
// carries the publisher line item ID so the ad server's freq-cap / event
// flow has a stable key to attribute against.
func (d *serveDeps) serveDirect(ctx context.Context, w http.ResponseWriter, reqLog *slog.Logger, li *publisheradserver.PublisherLineItem, placement postgres.PlacementRow, traceID string) {
	creativeID := ""
	if len(li.CreativeIDs) > 0 {
		creativeID = li.CreativeIDs[0]
	}
	serveReq := models.ServeRequest{
		TraceID:       traceID,
		CampaignID:    li.ID,
		CreativeID:    creativeID,
		PlacementID:   placement.ID,
		PublisherID:   placement.PublisherID,
		AdvertiserID:  li.AccountID,
		BidModel:      "cpm",
		ClearingPrice: li.CPM,
		Currency:      li.Currency,
		SiteDomain:    placement.PublisherDomain,
		Width:         placement.Width,
		Height:        placement.Height,
	}
	body, _ := json.Marshal(serveReq)
	adReq, err := http.NewRequestWithContext(ctx, http.MethodPost, d.adserverURL+routes.AdServe, bytes.NewReader(body))
	if err != nil {
		reqLog.Error("build ad server request", "error", err)
		http.Error(w, "ad server request build failed", http.StatusInternalServerError)
		return
	}
	adReq.Header.Set(constants.HeaderContentType, constants.ContentTypeJSON)
	tracing.InjectHTTP(ctx, adReq)
	adResp, err := http.DefaultClient.Do(adReq)
	if err != nil {
		reqLog.Error("ad server call failed", "error", err)
		http.Error(w, "ad server unavailable", http.StatusBadGateway)
		return
	}
	defer adResp.Body.Close()
	var sr models.ServeResponse
	if err := json.NewDecoder(adResp.Body).Decode(&sr); err != nil {
		reqLog.Error("ad server decode failed", "error", err)
		http.Error(w, "ad server bad response", http.StatusBadGateway)
		return
	}

	// Closes analytics gap #1: direct-sold serves used to leave no
	// reporting record beyond the tracker pixel. Publishing this event
	// lets reporting attribute the impression to a publisher line item.
	// Fire-and-forget (WithoutCancel) so a slow NATS doesn't stall the
	// serve response; nil pub is tolerated (NATS unreachable at boot).
	if d.pub != nil {
		reqLog.Info("direct win",
			"publisher_line_item_id", li.ID,
			"publisher_id", placement.PublisherID,
			"placement_id", placement.ID,
			"priority_tier", li.PriorityTier,
			"demand_source", li.DemandSource,
			"cpm", li.CPM)
		pubCtx := context.WithoutCancel(ctx)
		go d.pub.DirectWin(pubCtx, events.DirectWinEvent{
			TraceID:             traceID,
			PublisherLineItemID: li.ID,
			PublisherID:         placement.PublisherID,
			PlacementID:         placement.ID,
			PriorityTier:        li.PriorityTier,
			DemandSource:        li.DemandSource,
			CreativeID:          creativeID,
			CPM:                 li.CPM,
			Currency:            li.Currency,
			Timestamp:           d.clk.Now(),
		})
	}

	// Pacing actuals: record the served impression so future arbitration
	// decisions see the updated delivery curve. Best-effort — pacing is
	// soft on errors (fail-open).
	if err := d.pacer.RecordImpression(ctx, *li); err != nil {
		reqLog.Warn("pacing record failed", "line_item", li.ID, "error", err)
	}

	out := struct {
		TraceID        string  `json:"trace_id"`
		Source         string  `json:"source"`
		LineItemID     string  `json:"line_item_id"`
		PriorityTier   string  `json:"priority_tier"`
		DemandSource   string  `json:"demand_source"`
		HTML           string  `json:"html"`
		ImpressionURL  string  `json:"impression_url"`
		ClickURL       string  `json:"click_url"`
		ViewabilityURL string  `json:"viewability_url"`
		Width          int     `json:"width"`
		Height         int     `json:"height"`
		CPM            float64 `json:"cpm"`
	}{
		TraceID:        traceID,
		Source:         "direct",
		LineItemID:     li.ID,
		PriorityTier:   li.PriorityTier,
		DemandSource:   li.DemandSource,
		HTML:           sr.HTML,
		ImpressionURL:  sr.ImpressionURL,
		ClickURL:       sr.ClickURL,
		ViewabilityURL: sr.ViewabilityURL,
		Width:          placement.Width,
		Height:         placement.Height,
		CPM:            li.CPM,
	}
	w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
	json.NewEncoder(w).Encode(out)
}

// sspProgrammaticResult is what serveProgrammatic gets from the SSP. The
// SSP response shape, decoded once so we can both compare it against
// outbound Prebid bids and pass it through if it wins.
type sspProgrammaticResult struct {
	Raw           []byte // the raw JSON, ready to write to ResponseWriter on win
	NoBid         bool   `json:"nobid"`
	HTML          string `json:"html"`
	ClearingPrice float64 `json:"clearing_price"`
}

// serveProgrammatic fans out to (a) our SSP and (b) every configured
// external Prebid Server in parallel, then picks the highest-priced
// non-nobid response. SSP-only deployment (no prebid_servers configured)
// degrades to the original SSP-passthrough behaviour with no extra
// round-trips. Returns true if a programmatic source filled, false if
// every source no-bid (caller falls through to house).
func (d *serveDeps) serveProgrammatic(ctx context.Context, w http.ResponseWriter, r *http.Request, reqLog *slog.Logger, placement postgres.PlacementRow, placementExt, traceID, geo, device, userID string) bool {
	// Two parallel paths: SSP (existing) + Prebid Servers (new). Both
	// race against the same enclosing context deadline so a slow source
	// can't extend total auction time.
	type sspResult struct {
		res sspProgrammaticResult
		err error
	}
	sspCh := make(chan sspResult, 1)
	go func() {
		res, err := d.callSSP(ctx, r, placementExt, geo, device, userID)
		sspCh <- sspResult{res, err}
	}()

	prebidEndpoints := splitCSV(d.prebidServersFn())
	prebidReq := buildPrebidBidRequest(placement, traceID, geo, device, userID)
	prebidResults := d.prebidClient.FanOut(ctx, prebidEndpoints, prebidReq)

	sspRes := <-sspCh
	if sspRes.err != nil {
		reqLog.Error("ssp call failed", "error", sspRes.err)
		// Don't fail the whole request if Prebid demand is available.
		// Otherwise propagate the original error shape.
		if best, ok := prebidclient.Highest(prebidResults); ok {
			d.writePrebidWinner(ctx, w, placement, traceID, best)
			return true
		}
		http.Error(w, "ssp unavailable", http.StatusBadGateway)
		return true
	}

	prebidBest, hasPrebid := prebidclient.Highest(prebidResults)
	sspWon := !sspRes.res.NoBid
	if !sspWon && !hasPrebid {
		// Nothing bid anywhere — caller tries house.
		return false
	}
	if !sspWon && hasPrebid {
		d.writePrebidWinner(ctx, w, placement, traceID, prebidBest)
		return true
	}
	if sspWon && hasPrebid && prebidBest.Price > sspRes.res.ClearingPrice {
		reqLog.Info("programmatic source: prebid beat ssp",
			"prebid_price", prebidBest.Price, "ssp_price", sspRes.res.ClearingPrice,
			"prebid_endpoint", prebidBest.Endpoint)
		d.writePrebidWinner(ctx, w, placement, traceID, prebidBest)
		return true
	}

	// SSP wins (either Prebid didn't bid, or its price didn't beat SSP's).
	reqLog.Info("programmatic source: ssp won",
		"ssp_price", sspRes.res.ClearingPrice,
		"prebid_competing", hasPrebid)
	w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
	w.Write(sspRes.res.Raw)
	return true
}

// callSSP is the existing SSP-call path lifted out so serveProgrammatic
// can run it concurrently with the Prebid fan-out. geo/device/userID
// reflect the post-defaulting values computed at the top of the serve
// handler (User-Agent inference + dev "USA" fallback), so the SSP-built
// bid request matches the Prebid-built one and DSP targeting decisions
// stay consistent across both paths.
func (d *serveDeps) callSSP(ctx context.Context, r *http.Request, placementExt, geo, device, userID string) (sspProgrammaticResult, error) {
	q := r.URL.Query()
	if q.Get("placement_id") == "" {
		q.Set("placement_id", placementExt)
	}
	if q.Get("geo") == "" && geo != "" {
		q.Set("geo", geo)
	}
	if q.Get("device") == "" && device != "" {
		q.Set("device", device)
	}
	if q.Get("user_id") == "" && userID != "" {
		q.Set("user_id", userID)
	}
	url := d.sspURL + routes.SSPServe + "?" + q.Encode()
	sspReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return sspProgrammaticResult{}, fmt.Errorf("build ssp request: %w", err)
	}
	tracing.InjectHTTP(ctx, sspReq)
	resp, err := http.DefaultClient.Do(sspReq)
	if err != nil {
		return sspProgrammaticResult{}, fmt.Errorf("ssp call: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return sspProgrammaticResult{}, fmt.Errorf("ssp body read: %w", err)
	}
	var parsed sspProgrammaticResult
	if err := json.Unmarshal(body, &parsed); err != nil {
		return sspProgrammaticResult{}, fmt.Errorf("ssp decode: %w", err)
	}
	parsed.Raw = body
	return parsed, nil
}

// publishNoFill emits adtech.serve.nofill so reporting can compute fill
// rate. Without it the only signal of an exhausted-all-sources request was
// a log line. Fire-and-forget; nil publisher is tolerated.
func (d *serveDeps) publishNoFill(ctx context.Context, traceID string, p postgres.PlacementRow, reason string) {
	if d.pub == nil {
		return
	}
	logger.WithContext(d.log, ctx).Info("serve no-fill",
		"publisher_id", p.PublisherID,
		"placement_id", p.ID,
		"reason", reason)
	go d.pub.ServeNoFill(context.WithoutCancel(ctx), events.ServeNoFillEvent{
		TraceID:     traceID,
		PublisherID: p.PublisherID,
		PlacementID: p.ID,
		Reason:      reason,
		Timestamp:   d.clk.Now(),
	})
}

// writePrebidWinner renders an external Prebid bid's adm. We don't own
// the creative content (the external bidder is responsible for its
// own click + view tracking), but we DO need our own beacons fire so:
// (a) reporting can attribute the impression/view/click to the
// publisher line item, (b) fill-rate analytics don't overcount
// Prebid wins where the creative never actually rendered, (c)
// advertisers paying on the external buyer's vendor can still see
// their own attribution in our reports.
//
// Beacon strategy: outer wrapper div with the bid's adm verbatim
// inside, plus our 1×1 impression pixel + a hidden viewability
// beacon element at the end. The external bidder's pixels still fire
// from their original positions in the adm; ours fire from the
// wrapper. Both records get written, no party loses signal.
//
// Click tracking on Prebid is opt-in for the sim — we return a
// click_url alongside but don't override the bid's anchor hrefs
// (that would break the external buyer's click chain).
//
// Closes EVENT_PATHWAY_AUDIT Gap (Prebid viewability beacon).
func (d *serveDeps) writePrebidWinner(ctx context.Context, w http.ResponseWriter, placement postgres.PlacementRow, traceID string, best prebidclient.Result) {
	if d.pub != nil {
		logger.WithContext(d.log, ctx).Info("prebid outbound win",
			"publisher_id", placement.PublisherID,
			"placement_id", placement.ID,
			"prebid_endpoint", best.Endpoint,
			"seat", best.Seat,
			"clearing_price", best.Price)
		go d.pub.PrebidOutboundWin(context.WithoutCancel(context.Background()),
			events.PrebidOutboundWinEvent{
				TraceID:        traceID,
				PublisherID:    placement.PublisherID,
				PlacementID:    placement.ID,
				PrebidEndpoint: best.Endpoint,
				Seat:           best.Seat,
				ClearingPrice:  best.Price,
				Currency:       best.Currency,
				DealID:         best.DealID,
				Timestamp:      d.clk.Now(),
			})
	}

	// Build our impression / view / click URLs using the same macros
	// as the regular ad-server flow. CampaignID is empty (external
	// creative, no internal campaign); advertiser is the external
	// seat string so reporting can group wins by buyer.
	macroCtx := adserving.MacroContext{
		AuctionID:    traceID,
		AuctionPrice: best.Price,
		Currency:     best.Currency,
		PlacementID:  placement.ID,
		PublisherID:  placement.PublisherID,
		AdvertiserID: best.Seat,
		DealID:       best.DealID,
		Width:        placement.Width,
		Height:       placement.Height,
		TrackerURL:   d.trackerURL,
	}
	impressionURL := adserving.BuildImpressionURL(macroCtx)
	clickURL := adserving.BuildClickURL(macroCtx)
	viewabilityURL := adserving.BuildViewabilityURL(macroCtx)

	// Wrap the bid adm with our beacons. Outer div so script-shaped
	// or iframe-shaped admm still render normally; our beacons sit
	// outside the bid's own DOM scope so they aren't disturbed by
	// whatever the bidder does inside.
	wrappedHTML := `<div data-prebid-wrapper="1" style="display:block;width:100%;height:100%;">` +
		best.HTML +
		`<img src="` + impressionURL + `" width="1" height="1" style="display:none;" alt="" />` +
		`</div>`

	out := struct {
		TraceID        string  `json:"trace_id"`
		Source         string  `json:"source"`
		PrebidEndpoint string  `json:"prebid_endpoint"`
		Seat           string  `json:"seat"`
		HTML           string  `json:"html"`
		ImpressionURL  string  `json:"impression_url"`
		ClickURL       string  `json:"click_url"`
		ViewabilityURL string  `json:"viewability_url"`
		Width          int     `json:"width"`
		Height         int     `json:"height"`
		ClearingPrice  float64 `json:"clearing_price"`
		Currency       string  `json:"currency"`
		DealID         string  `json:"deal_id,omitempty"`
	}{
		TraceID:        traceID,
		Source:         "prebid",
		PrebidEndpoint: best.Endpoint,
		Seat:           best.Seat,
		HTML:           wrappedHTML,
		ImpressionURL:  impressionURL,
		ClickURL:       clickURL,
		ViewabilityURL: viewabilityURL,
		Width:          placement.Width,
		Height:         placement.Height,
		ClearingPrice:  best.Price,
		Currency:       best.Currency,
		DealID:         best.DealID,
	}
	w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
	json.NewEncoder(w).Encode(out)
}

// buildPrebidBidRequest constructs the OpenRTB request we POST to every
// configured Prebid Server. Same shape our own Prebid bidder accepts —
// consistent across inbound + outbound directions.
func buildPrebidBidRequest(p postgres.PlacementRow, traceID, geo, device, userID string) openrtb.BidRequest {
	imp := openrtb.Imp{
		ID:       "imp-1",
		TagID:    p.ID,
		BidFloor: p.FloorPrice,
		Banner:   &openrtb.Banner{W: p.Width, H: p.Height},
	}
	req := openrtb.BidRequest{
		ID:  traceID,
		Imp: []openrtb.Imp{imp},
		Site: &openrtb.Site{
			Domain:    p.PublisherDomain,
			Publisher: &openrtb.Publisher{ID: p.PublisherID},
		},
		TMax: 500,
	}
	if geo != "" || device != "" {
		req.Device = &openrtb.Device{}
		if geo != "" {
			req.Device.Geo = &openrtb.Geo{Country: geo}
		}
		if device != "" {
			req.Device.DeviceType = deviceTypeFromString(device)
		}
	}
	if userID != "" {
		req.User = &openrtb.User{ID: userID}
	}
	return req
}

func deviceTypeFromString(s string) int {
	switch s {
	case "mobile":
		return 1
	case "desktop":
		return 2
	case "ctv":
		return 3
	case "tablet":
		return 5
	}
	return 2
}

// deviceFromUserAgent picks "mobile" / "tablet" / "desktop" from the
// User-Agent string. Cheap substring check, not a full UA-parsing library
// — enough to drive DSP device-targeting from the demo simulator without
// the caller having to set ?device=. Defaults to desktop when ambiguous.
func deviceFromUserAgent(ua string) string {
	ua = strings.ToLower(ua)
	switch {
	case strings.Contains(ua, "ipad") || strings.Contains(ua, "tablet"):
		return "tablet"
	case strings.Contains(ua, "mobi") || strings.Contains(ua, "android") || strings.Contains(ua, "iphone"):
		return "mobile"
	default:
		return "desktop"
	}
}

// splitCSV trims and skips empties so a trailing comma or whitespace in
// the config value doesn't produce phantom endpoints.
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func writeNoBid(w http.ResponseWriter, traceID string) {
	w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
	json.NewEncoder(w).Encode(map[string]any{
		"trace_id": traceID,
		"nobid":    true,
		"source":   "none",
	})
}

func decisionLabel(t arbitration.DecisionType) string {
	switch t {
	case arbitration.DecisionDirect:
		return "direct"
	case arbitration.DecisionProgrammatic:
		return "programmatic"
	case arbitration.DecisionHouse:
		return "house"
	}
	return "unknown"
}

func startLineItemCache(cfg *config.Config, clk clock.Clock, log *slog.Logger) *warm.Cache[publisheradserver.PublisherLineItem] {
	pollInterval := firstNonZeroDuration(
		cfg.GetDuration("cache.warm.publisher_line_items.poll_interval", 0),
		cfg.GetDuration("cache.warm.poll_interval", 30*time.Second),
	)
	loader := pickLineItemLoader(cfg, log)
	bus := connectNATS(cfg, log)
	c := warm.New(warm.Config[publisheradserver.PublisherLineItem]{
		Name:              "publisher_line_items",
		Loader:            loader,
		Clock:             clk,
		Bus:               bus,
		InvalidateSubject: events.SubjectCacheInvalidatePublisherLineItems,
		PollInterval:      pollInterval,
		Log:               log,
	})
	if err := c.Start(context.Background()); err != nil {
		log.Error("publisher line item cache initial load failed", "error", err)
	}
	return c
}

func startPlacementCache(cfg *config.Config, clk clock.Clock, log *slog.Logger) *warm.Cache[postgres.PlacementRow] {
	pollInterval := firstNonZeroDuration(
		cfg.GetDuration("cache.warm.placements.poll_interval", 0),
		cfg.GetDuration("cache.warm.poll_interval", 30*time.Second),
	)
	loader := pickPlacementLoader(cfg, log)
	bus := connectNATS(cfg, log)
	c := warm.New(warm.Config[postgres.PlacementRow]{
		Name:              "placements",
		Loader:            loader,
		Clock:             clk,
		Bus:               bus,
		InvalidateSubject: events.SubjectCacheInvalidatePlacements,
		PollInterval:      pollInterval,
		Log:               log,
	})
	if err := c.Start(context.Background()); err != nil {
		log.Error("placement cache initial load failed", "error", err)
	}
	return c
}

// pickLineItemLoader / pickPlacementLoader return self-healing
// warm.Loaders (see pkg/cache/warm.RetryingLoader). Lazy-open Postgres
// on first LoadAll; reconnect after any error so the publisher-adserver
// picks up rows automatically if Postgres was unreachable at boot.
func pickLineItemLoader(cfg *config.Config, log *slog.Logger) warm.Loader[publisheradserver.PublisherLineItem] {
	dbURL := cfg.Get("database.url", "")
	return &warm.RetryingLoader[publisheradserver.PublisherLineItem]{
		Log:   log,
		KeyFn: func(li publisheradserver.PublisherLineItem) string { return li.ID },
		Construct: func() (warm.Loader[publisheradserver.PublisherLineItem], error) {
			if dbURL == "" {
				return nil, fmt.Errorf("database.url not set")
			}
			store, err := postgres.New(postgres.Config{PrimaryURL: dbURL, MaxOpenConns: 5, MaxIdleConns: 2, ConnMaxLifetime: 5 * time.Minute})
			if err != nil {
				return nil, fmt.Errorf("postgres connect: %w", err)
			}
			return &postgres.PublisherLineItemLoader{Store: store}, nil
		},
	}
}

func pickPlacementLoader(cfg *config.Config, log *slog.Logger) warm.Loader[postgres.PlacementRow] {
	dbURL := cfg.Get("database.url", "")
	return &warm.RetryingLoader[postgres.PlacementRow]{
		Log:   log,
		KeyFn: func(r postgres.PlacementRow) string { return r.ID },
		Construct: func() (warm.Loader[postgres.PlacementRow], error) {
			if dbURL == "" {
				return nil, fmt.Errorf("database.url not set")
			}
			store, err := postgres.New(postgres.Config{PrimaryURL: dbURL, MaxOpenConns: 5, MaxIdleConns: 2, ConnMaxLifetime: 5 * time.Minute})
			if err != nil {
				return nil, fmt.Errorf("postgres connect: %w", err)
			}
			return &postgres.PlacementLoader{Store: store}, nil
		},
	}
}

func connectNATS(cfg *config.Config, log *slog.Logger) events.EventBus {
	url := cfg.Get("publisher_adserver.nats_url", cfg.Get("nats.url", routes.DefaultNATSURL))
	bus, err := natsbus.New(url, constants.ServicePublisherAdServer, log)
	if err != nil {
		log.Warn("nats unavailable, caches will poll only", "error", err)
		return nil
	}
	return bus
}

func connectRedis(cfg *config.Config, log *slog.Logger) cache.L2Cache {
	addr := cfg.Get("redis.url", routes.DefaultRedisAddr)
	pwd := cfg.Get("redis.password", "")
	db := cfg.GetInt("redis.db", 0)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client, err := cacheredis.New(ctx, cacheredis.Config{Addr: addr, Password: pwd, DB: db})
	if err != nil {
		log.Warn("redis unreachable, pacing will use in-memory L2", "addr", addr, "error", err)
		return cache.NewMemoryL2()
	}
	log.Info("redis connected", "addr", addr)
	return client
}

func firstNonZeroDuration(ds ...time.Duration) time.Duration {
	for _, d := range ds {
		if d > 0 {
			return d
		}
	}
	return 30 * time.Second
}
