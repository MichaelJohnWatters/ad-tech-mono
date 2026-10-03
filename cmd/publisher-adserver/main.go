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
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adserving"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache"
	cacheredis "github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache/redis"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache/warm"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events/natsbus"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/health"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/houseads"
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
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/secrets"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tracing"
	_ "github.com/lib/pq"
)

func main() {
	clk := clock.Real{}
	log := logger.New(constants.ServicePublisherAdServer)
	sc := config.Setup(constants.ServicePublisherAdServer, keys.PublisherAdServerSchema(), log)
	cfg := sc.Cfg
	_ = sc
	hlth := health.New()
	lc := lifecycle.New(log)

	// Sign VAST/audio click-tracking URLs with the ACTIVE hmac_tracker key from
	// the secrets store (Phase I) — follows rotation live; the tracker validates
	// the overlap set. Falls back to adserving.DefaultSigningKey until configured.
	pubadSecrets := secrets.Start(context.Background(), cfg, clk, log, constants.ServicePublisherAdServer)
	lc.OnShutdown("pubad-secrets-cache", func(_ context.Context) error { pubadSecrets.Stop(); return nil })
	pubadSecrets.WatchActive(context.Background(), secrets.PurposeHMACTracker, 30*time.Second, adserving.SetActiveSigningKey)

	port := keys.PublisherAdServer.Port.Get(cfg)
	sspURL := keys.PublisherAdServer.SSPURL.Get(cfg)
	adserverURL := keys.PublisherAdServer.AdserverURL.Get(cfg)
	trackerURL := keys.PublisherAdServer.TrackerURL.Get(cfg)
	// secureBase is the HTTPS, browser-reachable base (scheme+host) swapped in
	// for media + beacons ONLY on requests arriving via the HTTPS ingress
	// (X-Forwarded-Proto: https). The localhost/bridge/e2e path (no such header)
	// is untouched — see secureBase() helpers. publicBase is the current media
	// base we rewrite FROM on the display passthrough (its HTML + beacons are
	// built downstream and come back as opaque strings).
	secureBase := keys.PublisherAdServer.PublicURLSecure.Get(cfg)

	otelShutdown := tracing.Init(context.Background(), tracing.Config{
		ServiceName:    constants.ServicePublisherAdServer,
		ServiceVersion: keys.Otel.ServiceVersion.Get(cfg),
		Endpoint:       keys.Otel.Endpoint.Get(cfg),
		SampleRatio:    keys.Otel.SampleRatio.Get(cfg),
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
	// House-ad cache: the platform's own fallback creatives, served on a no-bid
	// when stub_on_nobid is on. Same warm-cache shape as placements/line items
	// (poll + NATS invalidate). Not gated by a readiness check — an empty
	// house-ad set is a valid state (no configured house ads → honest no-fill),
	// so the cache never blocks readiness.
	houseAdCache := startHouseAdCache(cfg, clk, log)
	if houseAdCache != nil {
		lc.OnShutdown("house-ad-cache", func(_ context.Context) error { houseAdCache.Stop(); return nil })
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
	// On-demand profiler (internal mux only; zero cost until a profile is
	// pulled). Block/mutex profiling stays off until armed — see
	// middleware.SetProfileRates.
	middleware.AttachPprof(mux)
	mux.Handle(routes.Healthz, hlth.LivenessHandler())
	mux.Handle(routes.Readyz, hlth.ReadinessHandler())
	mux.Handle(routes.Metrics, metrics.Handler())

	if keys.Debug.EndpointsEnabled.Get(cfg) {
		mux.HandleFunc(routes.DebugCacheRefresh, warm.RefreshHandler(lineItemCache, placementCache, houseAdCache))
		mux.HandleFunc(routes.DebugPubAdLineItems, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
			json.NewEncoder(w).Encode(lineItemCache.All())
		})
	}

	// Outbound Prebid client. Timeout is re-read live so config edits to
	// publisher_adserver.prebid_timeout land without a restart, just like
	// exchange.bid_timeout does.
	prebidTimeout := keys.PublisherAdServer.PrebidTimeout.Get(cfg)
	prebidCli := prebidclient.New(prebidTimeout, log)
	prebidServersFn := func() string { return keys.PublisherAdServer.PrebidServers.Get(cfg) }

	// Event publisher for DirectWin + PrebidOutboundWin. Nil-tolerant —
	// if NATS is unreachable we skip publishing rather than failing the
	// serve. Closes the analytics blind spots where direct-sold serves
	// and external Prebid wins left no reporting record.
	natsURL := cfg.Get(keys.PublisherAdServer.NATSURL.Key(), keys.NATS.URL.Get(cfg))
	var pub *events.Publisher
	if pubBus, err := natsbus.New(natsURL, constants.ServicePublisherAdServer+"-events", log); err == nil {
		ctx := context.Background()
		pubBus.EnsureStreamWithRetry(ctx, events.StreamName, []string{events.StreamSubjects})
		pub = events.NewPublisher(pubBus, log)
		lc.OnShutdown("pubad-publisher", func(_ context.Context) error { return pubBus.Close() })
	} else {
		log.Warn("pubad event publisher unavailable, DirectWin / PrebidOutboundWin events will be skipped", "error", err)
	}

	// stubFn is the master on/off for the house-ad fallback on a no-bid. OFF by
	// default: the platform serves only real auctioned demand, so a no-bid
	// returns an honest empty no-fill rather than fake data (the real-data-only
	// rule). When ON, houseAdFn supplies the ops-configured house ad to serve
	// for the format — if none is configured the handler still falls back to
	// the honest no-fill (we never invent canned content).
	stubFn := func() bool { return keys.PublisherAdServer.StubOnNobid.Get(cfg) }
	houseAdFn := houseAdPicker(houseAdCache)
	// publisher_adserver.public_url is the current browser-reachable media/base
	// origin. On the display passthrough it's the base we rewrite FROM → secureBase
	// when the request arrives over the HTTPS ingress (see rewriteBaseIfSecure).
	publicBase := keys.PublisherAdServer.PublicURL.Get(cfg)
	mux.HandleFunc(routes.PublisherAdServe, serveHandler(serveDeps{
		log:             log,
		clk:             clk,
		lineItemCache:   lineItemCache,
		placementCache:  placementCache,
		pacer:           pacer,
		sspURL:          sspURL,
		adserverURL:     adserverURL,
		trackerURL:      trackerURL,
		secureBase:      secureBase,
		publicBase:      publicBase,
		prebidClient:    prebidCli,
		prebidServersFn: prebidServersFn,
		pub:             pub,
		stubFn:          stubFn,
		houseAdFn:       houseAdFn,
	}))
	omidFn := func() (string, string) {
		return keys.PublisherAdServer.OmidVendor.Get(cfg),
			keys.PublisherAdServer.OmidVerificationURL.Get(cfg)
	}
	mux.HandleFunc(routes.PublisherAdServeVAST, vastHandler(log, trackerURL, sspURL, secureBase, omidFn, stubFn, houseAdFn))
	// publisher_adserver.public_url is the browser-reachable origin the VMAP
	// schedule tells the player to call back into for each break's VAST.
	// Defaults to the gateway's local origin since every demo path runs through
	// it. (Reused above as the display-passthrough rewrite base.)
	mux.HandleFunc(routes.PublisherAdServeVMAP, vmapHandler(log, publicBase))
	mux.HandleFunc(routes.PublisherAdServeNative, nativeHandler(log, trackerURL, sspURL, secureBase, stubFn, houseAdFn))
	mux.HandleFunc(routes.PublisherAdServeAudio, audioHandler(log, trackerURL, sspURL, secureBase, stubFn, houseAdFn))

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
	log             *slog.Logger
	clk             clock.Clock
	lineItemCache   *warm.Cache[publisheradserver.PublisherLineItem]
	placementCache  *warm.Cache[postgres.PlacementRow]
	pacer           *pacing.Tracker
	sspURL          string
	adserverURL     string
	trackerURL      string
	secureBase      string // HTTPS browser-reachable base used for media+beacons on X-Forwarded-Proto: https requests
	publicBase      string // current media base (publisher_adserver.public_url) — the display passthrough rewrites FROM this
	prebidClient    *prebidclient.Client
	prebidServersFn func() string     // CSV; re-read per request for live-tunable demand-source list
	pub             *events.Publisher // nil-tolerant; emits DirectWin + PrebidOutboundWin events
	stubFn          func() bool       // house-ad fallback master on/off (publisher_adserver.stub_on_nobid)
	houseAdFn       houseAdLookup     // format→configured house ad; nil-tolerant
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
			d.serveDirect(ctx, r, w, reqLog, decision.LineItem, placement, traceID)
			return
		case arbitration.DecisionProgrammatic:
			if d.serveProgrammatic(ctx, w, r, reqLog, placement, placementExt, traceID, geo, device, userID) {
				return
			}
			// Programmatic returned no-bid — fall through to house. First the
			// PUBLISHER's own house line item (a direct-sold house campaign);
			// then, if the master switch is on, the PLATFORM's ops-configured
			// display house ad (the platform advertising its own business).
			house := arbitration.DecideHouse(req, d.lineItemCache.All())
			if house.Type == arbitration.DecisionDirect && house.LineItem != nil {
				d.serveDirect(ctx, r, w, reqLog, house.LineItem, placement, traceID)
				return
			}
			if d.serveDisplayHouseAd(r, w, reqLog, placement, traceID) {
				return
			}
			d.publishNoFill(ctx, traceID, placement, "programmatic-nobid-and-no-house")
			writeNoBid(w, traceID, "programmatic-nobid-and-no-house")
		default:
			d.publishNoFill(ctx, traceID, placement, "arbitration-default-branch")
			writeNoBid(w, traceID, "arbitration-default-branch")
		}
	}
}

// serveDirect renders a direct-sold line item by calling the ad server with
// a ServeRequest, then writes the result back to the visitor. CampaignID
// carries the publisher line item ID so the ad server's freq-cap / event
// flow has a stable key to attribute against.
func (d *serveDeps) serveDirect(ctx context.Context, r *http.Request, w http.ResponseWriter, reqLog *slog.Logger, li *publisheradserver.PublisherLineItem, placement postgres.PlacementRow, traceID string) {
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
		writeNoBid(w, traceID, "adserver_unavailable")
		return
	}
	defer adResp.Body.Close()
	var sr models.ServeResponse
	if err := json.NewDecoder(adResp.Body).Decode(&sr); err != nil {
		reqLog.Error("ad server decode failed", "error", err)
		writeNoBid(w, traceID, "adserver_bad_response")
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
		TraceID:      traceID,
		Source:       "direct",
		LineItemID:   li.ID,
		PriorityTier: li.PriorityTier,
		DemandSource: li.DemandSource,
		// The HTML (creative asset <img> src + baked-in beacons) and the three
		// sibling beacon URLs are built downstream by the ad server with http://
		// bases. On an HTTPS ingress request, re-base them so the browser on the
		// HTTPS demo page doesn't block them as mixed content; path+query (and so
		// the HMAC) are preserved. No-op on the localhost/bridge/e2e path.
		HTML:           rewriteBaseIfSecure(r, sr.HTML, d.publicBase, d.secureBase),
		ImpressionURL:  rewriteHostIfSecure(r, sr.ImpressionURL, d.secureBase),
		ClickURL:       rewriteHostIfSecure(r, sr.ClickURL, d.secureBase),
		ViewabilityURL: rewriteHostIfSecure(r, sr.ViewabilityURL, d.secureBase),
		Width:          placement.Width,
		Height:         placement.Height,
		CPM:            li.CPM,
	}
	outcome := adserving.Outcome{Result: adserving.OutcomeFill, Type: "display", Price: li.CPM, Currency: defaultStr2(li.Currency, "USD"), Model: "cpm", Reason: "direct-sold"}
	if li.DemandSource == "house" {
		outcome.Result = adserving.OutcomeHouse
		outcome.Reason = "house line item"
	}
	adserving.SetOutcome(w, outcome)
	w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
	json.NewEncoder(w).Encode(out)
}

// serveDisplayHouseAd renders the platform's ops-configured DISPLAY house ad
// as the no-bid fill, when the master switch (stubFn) is on and one is
// configured. Returns true when it served. The response mirrors a display
// fill (source "house") so the JS SDK renders the markup verbatim; house ads
// are platform content, not auctioned demand, so there are no impression /
// click / billing URLs. If the fallback is off or none is configured, returns
// false and the caller writes the honest no-fill.
func (d *serveDeps) serveDisplayHouseAd(r *http.Request, w http.ResponseWriter, reqLog *slog.Logger, placement postgres.PlacementRow, traceID string) bool {
	if d.stubFn == nil || !d.stubFn() || d.houseAdFn == nil {
		return false
	}
	ad, ok := d.houseAdFn(houseads.FormatDisplay, seedFromTrace(traceID))
	if !ok {
		reqLog.Info("display no-bid: house ads on but none configured for display")
		return false
	}
	reqLog.Info("display no-bid: serving configured house ad", "house_ad", ad.ID, "name", ad.Name)
	out := struct {
		TraceID string `json:"trace_id"`
		Source  string `json:"source"`
		HTML    string `json:"html"`
		Width   int    `json:"width"`
		Height  int    `json:"height"`
	}{TraceID: traceID, Source: "house", HTML: rewriteBaseIfSecure(r, ad.Markup, d.publicBase, d.secureBase), Width: placement.Width, Height: placement.Height}
	adserving.SetOutcome(w, adserving.Outcome{Result: adserving.OutcomeHouse, Type: "display", Reason: "no-demand"})
	w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
	json.NewEncoder(w).Encode(out)
	return true
}

// sspProgrammaticResult is what serveProgrammatic gets from the SSP. The
// SSP response shape, decoded once so we can both compare it against
// outbound Prebid bids and pass it through if it wins.
type sspProgrammaticResult struct {
	Raw           []byte  // the raw JSON, ready to write to ResponseWriter on win
	NoBid         bool    `json:"nobid"`
	HTML          string  `json:"html"`
	ClearingPrice float64 `json:"clearing_price"`
	// Segments: the consent-gated public audience segments the SSP echoed from
	// the bid request — stamped onto X-Adtech-Outcome on an SSP win so the
	// demosite trace panel can show the audience the auction ran with.
	Segments []string `json:"segments"`
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
			d.writePrebidWinner(ctx, r, w, placement, traceID, best)
			return true
		}
		writeNoBid(w, traceID, "ssp_unavailable")
		return true
	}

	prebidBest, hasPrebid := prebidclient.Highest(prebidResults)
	sspWon := !sspRes.res.NoBid
	if !sspWon && !hasPrebid {
		// Nothing bid anywhere — caller tries house.
		return false
	}
	if !sspWon && hasPrebid {
		d.writePrebidWinner(ctx, r, w, placement, traceID, prebidBest)
		return true
	}
	if sspWon && hasPrebid && prebidBest.Price > sspRes.res.ClearingPrice {
		reqLog.Info("programmatic source: prebid beat ssp",
			"prebid_price", prebidBest.Price, "ssp_price", sspRes.res.ClearingPrice,
			"prebid_endpoint", prebidBest.Endpoint)
		d.writePrebidWinner(ctx, r, w, placement, traceID, prebidBest)
		return true
	}

	// SSP wins (either Prebid didn't bid, or its price didn't beat SSP's).
	reqLog.Info("programmatic source: ssp won",
		"ssp_price", sspRes.res.ClearingPrice,
		"prebid_competing", hasPrebid)
	adserving.SetOutcome(w, adserving.Outcome{
		Result: adserving.OutcomeFill, Type: "display",
		Price: sspRes.res.ClearingPrice, Currency: "USD", Model: "cpm", Reason: "ssp",
		Segments: sspRes.res.Segments,
	})
	w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
	// SSP-win passthrough: the raw JSON carries html + impression/click/view
	// URLs + media_url built downstream with http:// bases. Re-base the whole
	// body (scheme+host only) on an HTTPS ingress request so nothing renders as
	// mixed content; the signed beacon path+params are unchanged. No-op on the
	// localhost/bridge/e2e path.
	w.Write([]byte(rewriteBaseIfSecure(r, string(sspRes.res.Raw), d.publicBase, d.secureBase)))
	return true
}

// forwardSSPQuery clones the visitor's incoming query params (geo, device, os,
// identity, consent + regulatory signals, segments) and forces the channel +
// placement so the SSP builds a production-like request for this format. Dev
// fallbacks fill geo/device only when the caller supplied none, matching the
// display path. This is what stops the video/native/VMAP paths from collapsing
// every visitor to a hardcoded USA/mobile request — the whole point of "no thin
// requests": a EU-no-consent CTV viewer must reach the DSP as one, not as a
// US desktop.
func forwardSSPQuery(incoming url.Values, channel, placementID, defaultDevice string) url.Values {
	q := url.Values{}
	for k, v := range incoming {
		q[k] = append([]string(nil), v...)
	}
	q.Set("channel", channel)
	if placementID != "" {
		q.Set("placement_id", placementID)
	}
	if q.Get("geo") == "" {
		q.Set("geo", "USA") // dev fallback, same as the display path
	}
	if q.Get("device") == "" && defaultDevice != "" {
		q.Set("device", defaultDevice)
	}
	return q
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
func (d *serveDeps) writePrebidWinner(ctx context.Context, r *http.Request, w http.ResponseWriter, placement postgres.PlacementRow, traceID string, best prebidclient.Result) {
	adserving.SetOutcome(w, adserving.Outcome{
		Result: adserving.OutcomeFill, Type: "display",
		Advertiser: best.Seat, Price: best.Price,
		Currency: defaultStr2(best.Currency, "USD"), Model: "cpm", Deal: best.DealID, Reason: "prebid",
	})
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
		// HTTPS ingress request → sign OUR beacons against the secure base so the
		// pixel/view/click fire from the HTTPS demo page without mixed-content
		// blocking. Base chosen BEFORE signing; path+params (the signed material)
		// are identical either way. No-op on the localhost/bridge/e2e path.
		TrackerURL: secureTrackerBase(r, d.trackerURL, d.secureBase),
	}
	impressionURL := adserving.BuildImpressionURL(macroCtx)
	clickURL := adserving.BuildClickURL(macroCtx)
	viewabilityURL := adserving.BuildViewabilityURL(macroCtx)

	// Wrap the bid adm with our beacons. Outer div so script-shaped
	// or iframe-shaped admm still render normally; our beacons sit
	// outside the bid's own DOM scope so they aren't disturbed by
	// whatever the bidder does inside.
	//
	// Two beacons ride along: the impression <img> (fires on render) AND a
	// self-contained viewability observer (fires our signed /v1/t/view once the
	// IAB display threshold — ≥50% on-screen for ≥1s — is met). The external adm
	// never loads web/static/adtech.js, so without this injected observer the
	// impression is recorded but the view never is — the exact "zero data
	// slippage" hole this path had.
	wrappedHTML := `<div data-prebid-wrapper="1" style="display:block;width:100%;height:100%;">` +
		best.HTML +
		`<img src="` + impressionURL + `" width="1" height="1" style="display:none;" alt="" />` +
		prebidViewabilityBeacon(viewabilityURL) +
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

// prebidViewabilityBeacon returns an inline <script> that observes its own
// wrapper element and fires the signed viewability URL once the IAB display
// threshold is met (≥50% of pixels on-screen for ≥1 continuous second),
// appending the client-measured dur/pct/area (the tracker excludes those from
// signature validation — see cmd/tracker viewSigParams). A faithful, no-SDK
// port of web/static/adtech.js observeViewability, so an external Prebid render
// self-reports viewability without loading our SDK. Uses an Image() GET (no
// CORS preflight, fires cross-origin) and document.currentScript to scope to
// its own ad when several render on one page.
func prebidViewabilityBeacon(viewabilityURL string) string {
	u, _ := json.Marshal(viewabilityURL) // safe JS string literal
	return `<script>(function(){` +
		`if(!('IntersectionObserver' in window))return;` +
		`var s=document.currentScript,w=s&&s.parentElement;if(!w)return;` +
		`var u=` + string(u) + `,t=null,done=false;` +
		`var o=new IntersectionObserver(function(es){var e=es[0];` +
		`if(e.isIntersecting&&e.intersectionRatio>=0.5){if(!t)t=Date.now();}else{t=null;}` +
		`if(!done&&t&&(Date.now()-t)>=1000){done=true;` +
		`var d=Date.now()-t,p=Math.round(e.intersectionRatio*100),a=w.offsetWidth*w.offsetHeight;` +
		`var sep=u.indexOf('?')===-1?'?':'&';` +
		`(new Image()).src=u+sep+'dur='+d+'&pct='+p+'&area='+a;o.disconnect();}` +
		`},{threshold:[0,0.5,1.0]});o.observe(w);})();</script>`
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

// writeNoBid sends the structured no-fill response. status is always 200
// so the SDK can branch on the body instead of a transport error code:
// HTTP errors signal infrastructure problems, "no_fill: true" signals
// "the auction ran but nobody bid / everyone was filtered". reason is a
// short machine-readable token (freqcap, no_eligible_campaigns,
// ssp_unavailable, …) the SDK can map to a publisher-friendly message
// without parsing log lines. Same shape across every no-fill path —
// gives ops a single field to filter / count by.
func writeNoBid(w http.ResponseWriter, traceID, reason string) {
	adserving.SetOutcome(w, adserving.Outcome{Result: adserving.OutcomeNoBid, Type: "display", Reason: reason})
	w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
	json.NewEncoder(w).Encode(map[string]any{
		"trace_id": traceID,
		"no_fill":  true,
		"nobid":    true, // legacy field kept for older e2e fixtures; drop after Phase 9
		"reason":   reason,
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
		cfg.GetDuration(keys.PublisherAdServer.WarmPublisherLineItemsPollInterval.Key(), 0),
		keys.CacheWarm.PollInterval.Get(cfg),
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
		cfg.GetDuration(keys.SSP.WarmPlacementsPollInterval.Key(), 0),
		keys.CacheWarm.PollInterval.Get(cfg),
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

// startHouseAdCache is the warm cache of the platform's own fallback creatives
// (house_ads). Mirrors the placement cache: poll + NATS invalidate on
// adtech.cache.invalidate.house-ads. Refreshes near-real-time when staff edit a
// house ad via the gateway.
func startHouseAdCache(cfg *config.Config, clk clock.Clock, log *slog.Logger) *warm.Cache[houseads.HouseAd] {
	loader := pickHouseAdLoader(cfg, log)
	bus := connectNATS(cfg, log)
	c := warm.New(warm.Config[houseads.HouseAd]{
		Name:              "house_ads",
		Loader:            loader,
		Clock:             clk,
		Bus:               bus,
		InvalidateSubject: events.SubjectCacheInvalidateHouseAds,
		PollInterval:      keys.CacheWarm.PollInterval.Get(cfg),
		Log:               log,
	})
	if err := c.Start(context.Background()); err != nil {
		log.Error("house ad cache initial load failed", "error", err)
	}
	return c
}

func pickHouseAdLoader(cfg *config.Config, log *slog.Logger) warm.Loader[houseads.HouseAd] {
	dbURL := cfg.Get(keys.Database.URL.Key(), "")
	return &warm.RetryingLoader[houseads.HouseAd]{
		Log:   log,
		KeyFn: func(h houseads.HouseAd) string { return h.ID },
		Construct: func() (warm.Loader[houseads.HouseAd], error) {
			if dbURL == "" {
				return nil, fmt.Errorf("database.url not set")
			}
			store, err := postgres.New(postgres.Config{PrimaryURL: dbURL, MaxOpenConns: 5, MaxIdleConns: 2, ConnMaxLifetime: 5 * time.Minute})
			if err != nil {
				return nil, fmt.Errorf("postgres connect: %w", err)
			}
			return &postgres.HouseAdLoader{Store: store}, nil
		},
	}
}

// houseAdLookup is the format→house-ad selector the no-bid serving paths call.
// Returns (ad, true) for a configured+enabled house ad of the format, or
// (_, false) → the handler serves an honest no-fill.
type houseAdLookup func(format string, seed uint64) (houseads.HouseAd, bool)

// seedFromTrace derives a deterministic picker seed from a trace ID (FNV-1a).
// Same trace → same house ad; distinct traces spread selection by weight. Never
// uses time / rand, which are banned for reproducibility.
func seedFromTrace(traceID string) uint64 {
	const (
		offset64 = 1469598103934665603
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	for i := 0; i < len(traceID); i++ {
		h ^= uint64(traceID[i])
		h *= prime64
	}
	return h
}

// houseAdPicker returns the format→house-ad lookup the serve handlers call on a
// no-bid. The seed makes selection deterministic (Math.random / time-based RNG
// is banned): the caller passes a trace-derived value so weighted rotation is
// reproducible in tests and spreads serving across eligible ads per weight. A
// nil cache (Postgres unreachable at boot) always returns not-found → honest
// no-fill.
func houseAdPicker(cache *warm.Cache[houseads.HouseAd]) func(format string, seed uint64) (houseads.HouseAd, bool) {
	return func(format string, seed uint64) (houseads.HouseAd, bool) {
		if cache == nil {
			return houseads.HouseAd{}, false
		}
		return houseads.Pick(cache.All(), format, seed)
	}
}

// pickLineItemLoader / pickPlacementLoader return self-healing
// warm.Loaders (see pkg/cache/warm.RetryingLoader). Lazy-open Postgres
// on first LoadAll; reconnect after any error so the publisher-adserver
// picks up rows automatically if Postgres was unreachable at boot.
func pickLineItemLoader(cfg *config.Config, log *slog.Logger) warm.Loader[publisheradserver.PublisherLineItem] {
	dbURL := cfg.Get(keys.Database.URL.Key(), "")
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
	dbURL := cfg.Get(keys.Database.URL.Key(), "")
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
	url := cfg.Get(keys.PublisherAdServer.NATSURL.Key(), keys.NATS.URL.Get(cfg))
	bus, err := natsbus.New(url, constants.ServicePublisherAdServer, log)
	if err != nil {
		log.Warn("nats unavailable, caches will poll only", "error", err)
		return nil
	}
	return bus
}

func connectRedis(cfg *config.Config, log *slog.Logger) cache.L2Cache {
	addr := keys.Redis.URL.Get(cfg)
	pwd := keys.Redis.Password.Get(cfg)
	db := keys.Redis.DB.Get(cfg)
	// Self-healing: a failed boot dial no longer latches MemoryL2 forever —
	// the wrapper serves fail-open from memory and swaps to Redis when the
	// background retry lands (pkg/cache/selfheal.go).
	return cache.NewSelfHealingL2(func(ctx context.Context) (cache.L2Cache, error) {
		return cacheredis.New(ctx, cacheredis.Config{Addr: addr, Password: pwd, DB: db})
	}, 10*time.Second, addr, log)
}

func firstNonZeroDuration(ds ...time.Duration) time.Duration {
	for _, d := range ds {
		if d > 0 {
			return d
		}
	}
	return 30 * time.Second
}
