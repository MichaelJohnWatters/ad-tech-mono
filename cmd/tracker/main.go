// cmd/tracker records ad events (impressions, clicks, conversions, viewability).
// Internet-facing - hit by end-user browsers via pixel URLs.
// Publishes events to NATS JetStream. Falls back to HTTP bridge if NATS unavailable.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adserving"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/ara"
	arapg "github.com/MichaelJohnWatters/ad-tech-mono/pkg/ara/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache/warm"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events/natsbus"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/fraud"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/health"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/identity"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/identityobserve"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/privacy"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/secrets"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tracing"
)

// 1x1 transparent GIF pixel (43 bytes)
var pixel = []byte{
	0x47, 0x49, 0x46, 0x38, 0x39, 0x61, 0x01, 0x00, 0x01, 0x00,
	0x80, 0x00, 0x00, 0xff, 0xff, 0xff, 0x00, 0x00, 0x00, 0x21,
	0xf9, 0x04, 0x01, 0x00, 0x00, 0x00, 0x00, 0x2c, 0x00, 0x00,
	0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0x02, 0x02, 0x44,
	0x01, 0x00, 0x3b,
}

func main() {
	log := logger.New(constants.ServiceTracker)
	// Seed override: signature validation defaults OFF in the schema
	// (phase-in affordance), but the local/prod deployments set
	// TRACKER_SIGNATURE_VALIDATION=true and that must reach the PER-POD
	// CONFIG ROW (TierLive rows outrank env at read time — without this
	// override the seeded 'false' row silently kept warn-mode forever,
	// which is how invalidly-signed VAST click URLs went unnoticed).
	setupOpts := []config.SetupOption{}
	if v := os.Getenv("TRACKER_SIGNATURE_VALIDATION"); v != "" {
		setupOpts = append(setupOpts, config.WithSeedDefaults(map[string]string{
			keys.Tracker.SignatureValidation.Key(): v,
		}))
	}
	sc := config.Setup(constants.ServiceTracker, keys.TrackerSchema(), log, setupOpts...)
	cfg := sc.Cfg
	// Live-tunable dedup knobs, consumed inside the Dedup constructor.
	dedupTTL := config.NewLiveDuration(sc.Manager, cfg, keys.Tracker.DedupTTL.Key(), keys.Tracker.DedupTTL.Default())
	dedupEnabled := config.NewLiveBool(sc.Manager, cfg, keys.Tracker.DedupEnabled.Key(), keys.Tracker.DedupEnabled.Default())
	hlth := health.New()
	lc := lifecycle.New(log)

	port := keys.Tracker.Port.Get(cfg)
	natsURL := keys.Tracker.NATSURL.Get(cfg)
	reportingURL := keys.Tracker.ReportingURL.Get(cfg)

	otelShutdown := tracing.Init(context.Background(), tracing.Config{
		ServiceName:    constants.ServiceTracker,
		ServiceVersion: keys.Otel.ServiceVersion.Get(cfg),
		Endpoint:       keys.Otel.Endpoint.Get(cfg),
		SampleRatio:    cfg.GetFloat(keys.Otel.SampleRatio.Key(), 0.1), // pixels are high volume — sample only 10% by default (platform default is 1.0)
		Log:            log,
	})
	lc.OnShutdown("otel", func(ctx context.Context) error { return otelShutdown(ctx) })

	// Try NATS first, fall back to HTTP bridge
	var bus events.EventBus
	natsBus, err := natsbus.New(natsURL, constants.ServiceTracker, log)
	if err != nil {
		log.Warn("nats unavailable, using HTTP bridge to reporting", "error", err)
		bus = nil // will use HTTP fallback
	} else {
		// Ensure the adtech stream exists. A failure here (NATS still booting
		// on a fresh-disk stack) must NOT latch bus=nil for the pod's life —
		// that latch is what SIGSEGVed all 3 tracker pods on 2026-08-05 (the
		// typed video/audio publishes hit the nil bus once beacons arrived)
		// and, pre-spool, silently rerouted a healthy pod to the HTTP bridge
		// forever. Keep the bus (the NATS client reconnects forever) and
		// retry the ensure in the background until it sticks; meanwhile
		// publishes fail into the disk spool and drain once the stream is up.
		// The retry helper embodies the fix (see the comment above): never
		// close the bus on a boot race, retry the ensure until it sticks.
		natsBus.EnsureStreamWithRetry(context.Background(), "adtech", []string{"adtech.>"})
		bus = natsBus
		lc.OnShutdown("nats", func(_ context.Context) error { return natsBus.Close() })
		log.Info("nats connected, publishing events to JetStream")
	}

	publisher := &eventPublisher{
		bus:          bus,
		typed:        events.NewPublisher(bus, log),
		identityPub:  identityobserve.NewPublisher(bus, log),
		reportingURL: reportingURL,
		log:          log,
	}
	// Event spool (money-critical tier): by the time a pixel fires, the ad
	// ALREADY displayed — refusing/failing the request cannot prevent the
	// event, only lose it. So the tracker never sheds; failed publishes go to
	// disk and replay on reconnect (dedup ids make replays billing-safe).
	// Spool metrics registered later once the metrics registry exists would
	// race the first request — so the tracker registers them on the default
	// path below with the shared registry.
	fraudChecker := fraud.NewRealTimeChecker(fraud.DefaultConfig())
	// Fraud-check client-IP resolver (shared trusted-proxy parser; hops read
	// live from tracker.trusted_proxy_hops). See clientip.go.
	clientIP := newClientIPFn(cfg)
	endUserIP := newEndUserIPFn(cfg)
	// DB-driven IP/UA blocklists, refreshed into the checker on a poll +
	// NATS invalidate. Nil when no database.url — hardcoded patterns remain.
	blocklistCache := startFraudBlocklistCache(cfg, log, bus, fraudChecker)
	if blocklistCache != nil {
		lc.OnShutdown("fraud-blocklist-cache", func(_ context.Context) error { blocklistCache.Stop(); return nil })
	}
	signingKey := keys.Tracker.SigningKey.Get(cfg)
	// Pixel-URL HMAC validation accepts the config signing key PLUS every
	// non-revoked hmac_tracker secret in the secrets store — the OVERLAP set for
	// key rotation. Add a new hmac_tracker secret (active) and pixels signed with
	// it validate immediately; its predecessor stays 'rotating' (still accepted)
	// until revoked, so pixels already in flight never break the instant a key
	// rotates. Rotations propagate via the secrets warm cache (NATS invalidate) —
	// no restart. Falls back to the config key alone when no secret is configured.
	secretsCache := secrets.Start(context.Background(), cfg, clock.Real{}, log, constants.ServiceTracker)
	lc.OnShutdown("tracker-secrets-cache", func(_ context.Context) error { secretsCache.Stop(); return nil })
	sigKeys := func() []string {
		ks := []string{signingKey}
		for _, s := range secretsCache.NonRevokedByPurpose(secrets.PurposeHMACTracker) {
			ks = append(ks, s.Value)
		}
		return ks
	}
	// sigKeysForAdvertiser is the validation key set for the S2S CONVERSION
	// postback — the one tracker beacon the ADVERTISER holds + signs, and the CPA
	// billing trigger. Per-advertiser keys (G7) stop a party holding the shared
	// platform key from forging a conversion billed to a DIFFERENT advertiser:
	//   - advertiser HAS its own hmac_conversion key(s) → validate against those.
	//       In grace (conversion_strict_advertiser_key=false) the shared platform
	//       key is ALSO accepted, so migration doesn't break in-flight postbacks;
	//       in strict mode ONLY the advertiser's key is accepted, closing the
	//       cross-advertiser forgery hole.
	//   - advertiser has NO issued key (unmigrated) → fall back to the platform
	//       key set, exactly as before, regardless of the strict flag.
	// advid is the advertiser account id carried on the conversion URL (== the
	// account CPA is billed to), so a forged advid can only ever be validated
	// against THAT advertiser's key.
	sigKeysForAdvertiser := func(advid string) []string {
		advKeys := secretsCache.NonRevokedByPurposeAndAccount(secrets.PurposeHMACConversion, advid)
		if advid == "" || len(advKeys) == 0 {
			return sigKeys() // unmigrated advertiser (or no advid) → platform key
		}
		ks := make([]string, 0, len(advKeys)+2)
		for _, s := range advKeys {
			ks = append(ks, s.Value)
		}
		if !keys.Tracker.ConversionStrictAdvKey.Get(cfg) {
			ks = append(ks, sigKeys()...) // grace: shared platform key still accepted
		}
		return ks
	}
	metrics := middleware.NewMetrics(constants.ServiceTracker)
	if bus != nil {
		if sp, err := events.NewSpool(events.SpoolDirFromEnv(), events.DefaultSpoolCap, metrics.Registry()); err != nil {
			log.Error("event spool init failed — publishes remain at-most-once", "error", err)
		} else {
			stopSpool := publisher.typed.EnableSpool(context.Background(), sp)
			lc.OnShutdown("event-spool", func(context.Context) error { stopSpool(); return nil })
			log.Info("event spool armed", "dir", events.SpoolDirFromEnv())
		}
	}

	l2 := connectRedis(cfg, log)
	dedup := NewDedup(l2, dedupTTL.Value, dedupEnabled.Value, log)

	mux := http.NewServeMux()
	// On-demand profiler (internal mux only; zero cost until a profile is
	// pulled). Block/mutex profiling stays off until armed — see
	// middleware.SetProfileRates.
	middleware.AttachPprof(mux)
	mux.Handle(routes.Healthz, hlth.LivenessHandler())
	mux.Handle(routes.Readyz, hlth.ReadinessHandler())
	mux.Handle(routes.Metrics, metrics.Handler())

	// Privacy Sandbox ARA (reporting-only overlay; docs/attribution-phase4-ara.md).
	// Best-effort DB — if it can't connect the endpoints 503 on use but the rest
	// of the tracker is unaffected. Source/trigger registration is gated by
	// tracker.ara_enabled + consent; the report-ingest endpoints stay mounted.
	araH := araDeps{enabled: func() bool { return keys.Tracker.ARAEnabled.Get(cfg) }, log: log, maxBody: 128 << 10}
	if dbURL := cfg.Get(keys.Database.URL.Key(), ""); dbURL != "" {
		if st, err := postgres.New(postgres.Config{PrimaryURL: dbURL, MaxOpenConns: 4, MaxIdleConns: 2, ConnMaxLifetime: 5 * time.Minute}); err != nil {
			log.Warn("ara: db connect failed; ARA endpoints will 503 on use", "error", err)
		} else {
			araH.store = arapg.New(st.Primary())
			lc.OnShutdown("ara-db", func(context.Context) error { return st.Close() })
		}
	}
	mux.HandleFunc(routes.TrackerARASource, araH.registerSource)
	mux.HandleFunc(ara.PathEventReport, araH.ingestEvent)
	mux.HandleFunc(ara.PathAggregateReport, araH.ingestAggregate)

	// Debug-gated synchronous cache refresh — lets the e2e harness force a
	// reload after inserting a fraud_blocklists row.
	if keys.Debug.EndpointsEnabled.Get(cfg) && blocklistCache != nil {
		mux.HandleFunc(routes.DebugCacheRefresh, warm.RefreshHandler(blocklistCache))
	}

	// Impression pixel
	mux.HandleFunc(routes.TrackerImpression, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		traceID := q.Get("tid")
		ctx := logger.WithTraceID(r.Context(), traceID)
		reqLog := logger.WithContext(log, ctx)

		// Validate HMAC signature. When tracker.signature_validation is
		// true (prod-shape) we 403 the request; otherwise we warn and let
		// the event through so dev pipelines that don't yet sign keep
		// flowing — but an unverified beacon never gets to assert WHO it
		// belongs to (attribution params are blanked below), or anyone
		// could forge impressions into a victim tenant's reports/billing.
		// The config knob is live-tunable so ops can ratchet strictness
		// without a redeploy.
		sigOK := adserving.ValidateSignatureAny(r.URL.Path, q, sigKeys())
		if !sigOK {
			reqLog.Warn("invalid signature", "path", r.URL.Path)
			if keys.Tracker.SignatureValidation.Get(cfg) {
				go publisher.publishRejected(context.WithoutCancel(ctx),
					"impression", "invalid_signature", "", traceID, reqLog)
				http.Error(w, "invalid signature", http.StatusForbidden)
				return
			}
		}

		if keys.Tracker.ExpValidation.Get(cfg) && isExpired(q, time.Now()) {
			reqLog.Warn("expired url", "path", r.URL.Path, "exp", q.Get("exp"))
			go publisher.publishRejected(context.WithoutCancel(ctx),
				"impression", "expired", q.Get("exp"), traceID, reqLog)
			http.Error(w, "url expired", http.StatusGone)
			return
		}

		// Real-time fraud check
		fraudResult := fraudChecker.Check(fraud.Request{
			IP: clientIP(r), UserAgent: r.UserAgent(),
			TraceID: traceID, Referer: r.Referer(),
		})
		// Dev-mode override: publisher simulator can append ?dev_force_fraud=1
		// to deliberately trip a block, so the UI can demonstrate the
		// fraud-rejection flow. Gated by debug.endpoints_enabled — prod
		// requests can't be forced into the blocked path by a forged param.
		if q.Get("dev_force_fraud") == "1" && keys.Debug.EndpointsEnabled.Get(cfg) {
			fraudResult.Blocked = true
			fraudResult.Reasons = append([]string{"dev_force_fraud"}, fraudResult.Reasons...)
		}
		if fraudResult.Blocked {
			reqLog.Warn("fraud blocked", "score", fraudResult.Score, "reasons", fraudResult.Reasons)
			go publisher.publishRejected(context.WithoutCancel(ctx),
				"impression", "fraud", strings.Join(fraudResult.Reasons, ","), traceID, reqLog)
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeGIF)
			// Dev-mode signal so the pub sim UI can show fraud was tripped.
			// Real bots get the silent pixel-return treatment in prod (this
			// header simply isn't set when debug endpoints are off).
			if keys.Debug.EndpointsEnabled.Get(cfg) {
				w.Header().Set("X-Dev-Fraud-Blocked", "1")
				w.Header().Set("X-Dev-Fraud-Reasons", strings.Join(fraudResult.Reasons, ","))
			}
			w.Write(pixel) // still return pixel (don't reveal detection)
			return         // but don't record or bill
		}

		price, _ := strconv.ParseFloat(q.Get("price"), 64)
		reqLog.Info("impression",
			"campaign_id", q.Get("cid"),
			"creative_id", q.Get("crid"),
			"placement_id", q.Get("pid"),
			"publisher_id", q.Get("pubid"),
			"price", price,
		)

		if !dedup.FirstSeen(ctx, "impression", traceID) {
			reqLog.Debug("duplicate impression, dropping", "trace_id", traceID)
			go publisher.publishRejected(context.WithoutCancel(ctx),
				"impression", "dedup", "", traceID, reqLog)
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeGIF)
			w.Header().Set(constants.HeaderCacheControl, constants.CacheNoStore)
			w.Write(pixel)
			return
		}

		// BidModel carried on the URL via ?bm= so the billing engine can
		// route reserve-vs-bill-immediately correctly. Default CPM when
		// missing (legacy URLs from before this param landed).
		bidModel := q.Get("bm")
		if bidModel == "" {
			bidModel = constants.BidModelCPM
		}

		// price is the auction CPM (OpenRTB clearing price = cost per 1000
		// impressions) signed into the beacon. Convert to the realized
		// per-impression cost HERE at the source, so every downstream consumer
		// — reporting→ClickHouse (hot), pipeline→Delta lake (cold), and the
		// billing ledger — books the same per-impression dollars. (Was
		// previously converted only in the reporting consumer, so the lake kept
		// the raw CPM and hot/cold disagreed 1000×.)
		impCost := price / 1000
		// DOOH audience multiplier: a screen's single proof-of-play delivers `mult`
		// impressions to the people in front of it. mult rides INSIDE the signed
		// beacon (the ad server bakes it per venue), so it can't be forged, and only
		// applies to the dooh channel. The event then represents `qty` impressions at
		// the full play cost (per-impression cost × qty) — billing books the play,
		// reporting sums the audience. Every other format is 1:1 (qty=1, cost=impCost).
		qty := 1
		if channelOrDefault(q.Get("ch")) == constants.ChannelDOOH {
			if m, err := strconv.Atoi(q.Get("mult")); err == nil && m > 1 {
				qty = m
			}
		}
		// Attribution (who gets billed/credited) only from signature-verified
		// URLs — same rule as the media beacons (see attribGate).
		attrib := attribGate(q, sigOK)
		go publisher.publishImpression(context.WithoutCancel(ctx), analytics.ImpressionEvent{
			TraceID:          traceID,
			CampaignID:       attrib("cid"),
			CreativeID:       attrib("crid"),
			PlacementID:      attrib("pid"),
			PublisherID:      attrib("pubid"),
			AccountID:        attrib("advid"),
			Geo:              q.Get("geo"),
			Device:           q.Get("dev"),
			Channel:          channelOrDefault(q.Get("ch")),
			ClearingPrice:    impCost, // per-impression (CPM/1000)
			ClearingCurrency: q.Get("cur"),
			ClearingPriceUSD: impCost * float64(qty), // full play cost — billing books this
			ImpressionQty:    qty,                    // audience impressions this play delivered
			BidModel:         bidModel,
			DealID:           attrib("deal"),
			SchemaVersion:    1,
			Timestamp:        time.Now().UTC(),
		}, reqLog)
		go publisher.publishBehaviour(context.WithoutCancel(ctx), "impression", q, reqLog)

		w.Header().Set(constants.HeaderContentType, constants.ContentTypeGIF)
		w.Header().Set(constants.HeaderCacheControl, constants.CacheNoStore)
		w.Write(pixel)
	})

	// Click redirect
	mux.HandleFunc(routes.TrackerClick, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		traceID := q.Get("tid")
		redir := q.Get("redir")
		ctx := logger.WithTraceID(r.Context(), traceID)
		reqLog := logger.WithContext(log, ctx)
		reqLog.Info("click", "campaign_id", q.Get("cid"), "redirect", redir)

		// HMAC validates that the URL was issued by us and hasn't been
		// tampered. Particularly important on click because the tracker
		// 302s to the redir param — without sig validation an attacker
		// could rewrite redir to a phishing landing page and use the
		// tracker as an open redirect.
		if !adserving.ValidateSignatureAny(r.URL.Path, q, sigKeys()) {
			reqLog.Warn("invalid signature", "path", r.URL.Path)
			if keys.Tracker.SignatureValidation.Get(cfg) {
				go publisher.publishRejected(context.WithoutCancel(ctx),
					"click", "invalid_signature", "", traceID, reqLog)
				http.Error(w, "invalid signature", http.StatusForbidden)
				return
			}
		}

		if keys.Tracker.ExpValidation.Get(cfg) && isExpired(q, time.Now()) {
			reqLog.Warn("expired url", "path", r.URL.Path, "exp", q.Get("exp"))
			go publisher.publishRejected(context.WithoutCancel(ctx),
				"click", "expired", q.Get("exp"), traceID, reqLog)
			http.Error(w, "url expired", http.StatusGone)
			return
		}

		if !dedup.FirstSeen(ctx, "click", traceID) {
			reqLog.Debug("duplicate click, dropping", "trace_id", traceID)
			go publisher.publishRejected(context.WithoutCancel(ctx),
				"click", "dedup", "", traceID, reqLog)
			if redir != "" {
				http.Redirect(w, r, appendTraceQuery(redir, traceID), http.StatusFound)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}

		go publisher.publishClick(context.WithoutCancel(ctx), analytics.ClickEvent{
			TraceID:     traceID,
			CampaignID:  q.Get("cid"),
			CreativeID:  q.Get("crid"),
			PlacementID: q.Get("pid"),
			PublisherID: q.Get("pubid"),
			AccountID:   q.Get("advid"),
			Geo:         q.Get("geo"),
			Device:      q.Get("dev"),
			LandingURL:  redir,
			Timestamp:   time.Now().UTC(),
		}, reqLog)
		go publisher.publishBehaviour(context.WithoutCancel(ctx), "click", q, reqLog)

		if redir == "" {
			http.Error(w, "missing redirect URL", http.StatusBadRequest)
			return
		}
		http.Redirect(w, r, appendTraceQuery(redir, traceID), http.StatusFound)
	})

	// Conversion pixel
	// Retargeting pixel — embedded on ADVERTISER sites (not our serving
	// chain), so there is no HMAC (a third-party page can't sign) and no
	// serve context. Consent is evaluated HERE from the pixel's regulatory
	// params (unlike serve beacons, where the consented uid was baked in by
	// the ad server). The pixel always renders regardless of capture, so
	// the page can't observe the consent decision. Worst-case abuse is an
	// advertiser polluting its OWN retargeting pool (rows are scoped to the
	// aid account at rule evaluation).
	mux.HandleFunc(routes.TrackerRetarget, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		// A retargeting site visit has no upstream auction, so it carries no
		// baked-in tid. The tracker mints the trace via HTTPMiddleware (a real
		// 32-hex OTel id); fall back to that when the beacon has no tid, so this
		// visit's trace_id is always a real trace — never a client-invented value.
		traceID := firstNonEmpty(q.Get("tid"), tracing.TraceIDFromContext(r.Context()))
		ctx := logger.WithTraceID(r.Context(), traceID)
		reqLog := logger.WithContext(log, ctx)
		uid, aid := q.Get("uid"), q.Get("aid")
		if uid != "" && aid != "" &&
			privacy.Evaluate(privacy.SignalsFromQuery(q.Get, r.Header.Get("Sec-GPC"))).Personalise {
			// Derive the shopper's HOUSEHOLD id (salted-IP hash — same salt +
			// derivation as the SSP so ids line up platform-wide) and stamp it
			// on the site_visit. audience-rt can then enroll the household
			// alongside the visitor id, which is what makes an ANONYMOUS guest
			// cart chaseable on the home's other devices (CTV included) with
			// no email bridge. End-user IP resolution mirrors the SSP:
			// trusted-proxy parse + allowlist-gated ?ip= (see clientip.go).
			if hh := identity.HouseholdID(keys.SSP.HouseholdSalt.Get(cfg), endUserIP(r)); hh != "" {
				q.Set("hh", hh)
			}
			go publisher.publishBehaviour(context.WithoutCancel(ctx), "site_visit", q, reqLog)
			// Link the visitor id to its hashed email (view-through bridge) and
			// household (purchase-suppression expansion for anonymous guests).
			publisher.publishAdvertiserIdentity(traceID, uid, q.Get("he"), q.Get("hh"))
		}
		w.Header().Set(constants.HeaderContentType, constants.ContentTypeGIF)
		w.Header().Set(constants.HeaderCacheControl, constants.CacheNoStore)
		w.Write(pixel)
	})

	mux.HandleFunc(routes.TrackerConversion, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		traceID := q.Get("tid")
		convType := q.Get("type")
		revenue, _ := strconv.ParseFloat(q.Get("rev"), 64)
		ctx := logger.WithTraceID(r.Context(), traceID)
		reqLog := logger.WithContext(log, ctx)
		reqLog.Info("conversion", "type", convType, "revenue", revenue)

		// HMAC validates the URL was issued by us. Conversions are the
		// CPA billing trigger so an unsigned conv URL is a direct
		// billing-fraud surface: anyone could fire /v1/t/conv with
		// arbitrary cid/crid/rev and rack up spend on a campaign that
		// didn't actually convert. Validated against the SIGNING ADVERTISER's
		// own key (by advid) so a party holding another advertiser's key can't
		// forge a conversion billed to this one (G7).
		if !adserving.ValidateSignatureAny(r.URL.Path, q, sigKeysForAdvertiser(q.Get("advid"))) {
			reqLog.Warn("invalid signature", "path", r.URL.Path)
			if keys.Tracker.SignatureValidation.Get(cfg) {
				go publisher.publishRejected(context.WithoutCancel(ctx),
					"conversion", "invalid_signature", convType, traceID, reqLog)
				http.Error(w, "invalid signature", http.StatusForbidden)
				return
			}
		}

		if keys.Tracker.ExpValidation.Get(cfg) && isExpired(q, time.Now()) {
			reqLog.Warn("expired url", "path", r.URL.Path, "exp", q.Get("exp"))
			go publisher.publishRejected(context.WithoutCancel(ctx),
				"conversion", "expired", q.Get("exp"), traceID, reqLog)
			http.Error(w, "url expired", http.StatusGone)
			return
		}

		if !dedup.FirstSeen(ctx, "conversion:"+convType, traceID) {
			reqLog.Debug("duplicate conversion, dropping", "trace_id", traceID, "type", convType)
			go publisher.publishRejected(context.WithoutCancel(ctx),
				"conversion", "dedup", convType, traceID, reqLog)
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeGIF)
			w.Header().Set(constants.HeaderCacheControl, constants.CacheNoStore)
			w.Write(pixel)
			return
		}

		// ctid = the earning exposure's trace (the click/impression that carried
		// the CPA reservation), captured on the advertiser's landing page and
		// returned on the signed postback. It rides INSIDE the HMAC-signed URL,
		// so it can't be forged. Present => deterministic click-through
		// attribution; absent => unattributed (reporting falls back to settling
		// against the conversion's own trace, which normally finds no reservation).
		attributedTrace := q.Get("ctid")
		attrType := ""
		if attributedTrace != "" {
			attrType = "click_through"
		}
		go publisher.publishConversion(context.WithoutCancel(ctx), analytics.ConversionEvent{
			TraceID:           traceID,
			CampaignID:        q.Get("cid"),
			CreativeID:        q.Get("crid"),
			PlacementID:       q.Get("pid"),
			AccountID:         q.Get("advid"),
			ConversionType:    convType,
			Revenue:           revenue,
			Currency:          q.Get("cur"),
			RevenueUSD:        revenue,
			AttributedTraceID: attributedTrace,
			AttributionType:   attrType,
			UserID:            q.Get("uid"),
			// DPA: purchased SKUs (sku= single or skus= CSV) drive per-product
			// suppression + cross-sell in audience-rt.
			SKUs:      firstNonEmpty(q.Get("skus"), q.Get("sku")),
			Timestamp: time.Now().UTC(),
		}, reqLog)
		go publisher.publishBehaviour(context.WithoutCancel(ctx), "conversion", q, reqLog)

		// ARA: a conversion also registers a Privacy Sandbox trigger (consent-gated,
		// tracker.ara_enabled). Reporting-only overlay — the deterministic
		// attribution above is untouched.
		araH.setTriggerHeader(w, r, convType, revenue)
		w.Header().Set(constants.HeaderContentType, constants.ContentTypeGIF)
		w.Header().Set(constants.HeaderCacheControl, constants.CacheNoStore)
		w.Write(pixel)
	})

	// Viewability beacon. Client (adtech.js / publisher simulator) calls
	// this after observing the rendered ad in the viewport. URL params:
	//   tid  - trace id (links back to the impression)
	//   cid, crid, pid, pubid, advid - campaign / creative / placement /
	//                                  publisher / advertiser
	//   dur  - milliseconds the ad was visible at >= the IAB threshold
	//   pct  - peak percent of the ad's pixels in viewport (0-100)
	//   area - optional, ad's pixel area (w*h). When provided, the >=242,500
	//          IAB Large Format rule kicks in (30% threshold instead of 50%).
	//
	// Server is the authority on IsIABViewable — the client's bool isn't
	// trusted. The analytics row stores both the measured inputs and the
	// server's verdict so downstream consumers (billing, dashboards) can
	// filter on iab_viewable directly.
	mux.HandleFunc(routes.TrackerView, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		traceID := q.Get("tid")
		ctx := logger.WithTraceID(r.Context(), traceID)
		reqLog := logger.WithContext(log, ctx)

		// Same HMAC + fraud + dedup gates as the impression handler — but the
		// viewability MEASUREMENT params (dur/pct/area) are computed client-side
		// and appended AFTER the URL was signed (see web/static/adtech.js
		// observeViewability and the injected Prebid beacon), so they were never
		// part of the signed message. Exclude them from validation, or every real
		// viewability beacon would 403 under strict signing (the signed URL only
		// covers tid/cid/pid/pubid/uid/exp — see BuildViewabilityURL).
		if !adserving.ValidateSignatureAny(r.URL.Path, viewSigParams(q), sigKeys()) {
			reqLog.Warn("invalid signature", "path", r.URL.Path)
			if keys.Tracker.SignatureValidation.Get(cfg) {
				go publisher.publishRejected(context.WithoutCancel(ctx),
					"view", "invalid_signature", "", traceID, reqLog)
				http.Error(w, "invalid signature", http.StatusForbidden)
				return
			}
		}

		if keys.Tracker.ExpValidation.Get(cfg) && isExpired(q, time.Now()) {
			reqLog.Warn("expired url", "path", r.URL.Path, "exp", q.Get("exp"))
			go publisher.publishRejected(context.WithoutCancel(ctx),
				"view", "expired", q.Get("exp"), traceID, reqLog)
			http.Error(w, "url expired", http.StatusGone)
			return
		}

		fraudResult := fraudChecker.Check(fraud.Request{
			IP: clientIP(r), UserAgent: r.UserAgent(),
			TraceID: traceID, Referer: r.Referer(),
		})
		if fraudResult.Blocked {
			reqLog.Warn("view fraud blocked", "score", fraudResult.Score, "reasons", fraudResult.Reasons)
			go publisher.publishRejected(context.WithoutCancel(ctx),
				"view", "fraud", strings.Join(fraudResult.Reasons, ","), traceID, reqLog)
			w.WriteHeader(http.StatusNoContent)
			return
		}

		durMs, _ := strconv.ParseInt(q.Get("dur"), 10, 64)
		pct, _ := strconv.Atoi(q.Get("pct"))
		areaPx, _ := strconv.ParseInt(q.Get("area"), 10, 64)
		channel := q.Get("ch") // "video" → 2s IAB dwell; empty/display → 1s
		iabViewable := analytics.IsIABViewable(durMs, pct, areaPx, channel)

		reqLog.Info("viewability",
			"duration_ms", durMs,
			"percent_visible", pct,
			"area_px", areaPx,
			"iab_viewable", iabViewable,
			"campaign_id", q.Get("cid"),
		)

		// One view per (trace, view) — multiple beacons on the same render
		// (browser back-button replay, double-firing IntersectionObserver) get
		// deduped here, same as impressions.
		if !dedup.FirstSeen(ctx, "view", traceID) {
			reqLog.Debug("duplicate view, dropping", "trace_id", traceID)
			go publisher.publishRejected(context.WithoutCancel(ctx),
				"view", "dedup", "", traceID, reqLog)
			w.WriteHeader(http.StatusNoContent)
			return
		}

		go publisher.publishView(context.WithoutCancel(ctx), analytics.ViewEvent{
			TraceID:        traceID,
			CampaignID:     q.Get("cid"),
			CreativeID:     q.Get("crid"),
			PlacementID:    q.Get("pid"),
			PublisherID:    q.Get("pubid"),
			AccountID:      q.Get("advid"),
			Channel:        channel,
			DurationMs:     durMs,
			PercentVisible: pct,
			AreaPx:         areaPx,
			IABViewable:    iabViewable,
			SchemaVersion:  1,
			Timestamp:      time.Now().UTC(),
		}, reqLog)
		go publisher.publishBehaviour(context.WithoutCancel(ctx), "view", q, reqLog)

		// Echo the server's verdict back to the client (the simulator reads
		// this so it can show "IAB viewable (server-verified)" vs the JS-only
		// guess). Header-only — body stays 204.
		if iabViewable {
			w.Header().Set("X-IAB-Viewable", "1")
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// Shared gate for the /v1/t/video + /v1/t/audio beacons: the same
	// sig → expiry → fraud → dedup checks the impression pixel runs, with the
	// same live-tunable strictness knobs. See mediagate.go.
	mediaGate := mediaEventGate{
		sigKeys:       sigKeys,
		sigValidation: func() bool { return keys.Tracker.SignatureValidation.Get(cfg) },
		expValidation: func() bool { return keys.Tracker.ExpValidation.Get(cfg) },
		fraud:         fraudChecker,
		clientIP:      clientIP,
		dedup:         dedup,
		publisher:     publisher,
	}

	// Video/Audio events. Gated identically to the impression pixel (sig →
	// fraud → dedup). Publish through the same eventPublisher — bus may be nil
	// in dev-no-NATS mode, in which case the publish is a no-op and we still
	// return 204 so the player keeps firing pings.
	mux.HandleFunc(routes.TrackerVideo, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		traceID := q.Get("tid")
		eventType := q.Get("event")
		ctx := logger.WithTraceID(r.Context(), traceID)
		reqLog := logger.WithContext(log, ctx)
		reqLog.Info("video_event", "event_type", eventType)
		record, sigOK := mediaGate.allow(w, r, "video", eventType, traceID, reqLog)
		if !record {
			return
		}
		go publisher.publishVideo(context.WithoutCancel(ctx),
			videoEventFromQuery(q, traceID, eventType, sigOK, time.Now()), reqLog)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc(routes.TrackerAudio, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		traceID := q.Get("tid")
		eventType := q.Get("event")
		ctx := logger.WithTraceID(r.Context(), traceID)
		reqLog := logger.WithContext(log, ctx)
		reqLog.Info("audio_event", "event_type", eventType)
		record, sigOK := mediaGate.allow(w, r, "audio", eventType, traceID, reqLog)
		if !record {
			return
		}
		go publisher.publishAudio(context.WithoutCancel(ctx),
			audioEventFromQuery(q, traceID, eventType, sigOK, time.Now()), reqLog)
		w.WriteHeader(http.StatusNoContent)
	})

	// Per-IP rate limit on the pixel endpoints (off by default — forgery is
	// already blocked by HMAC signature validation; this is a live-tunable
	// volumetric floor). Allowlist + infra paths bypass; spoof-resistant XFF.
	trkRL := middleware.NewLiveRateLimiter(func() middleware.RateLimitConfig {
		return middleware.RateLimitConfig{
			RPS:         keys.Tracker.RateLimitRPS.Get(cfg),
			Burst:       keys.Tracker.RateLimitBurst.Get(cfg),
			TrustedHops: keys.Tracker.RateLimitTrustedHops.Get(cfg),
			Allowlist:   keys.Tracker.RateLimitAllowlist.Get(cfg),
			Distributed: keys.Tracker.RateLimitDistributed.Get(cfg),
		}
	}, log).WithDistributedBackend(constants.ServiceTracker, l2)
	handler := tracing.HTTPMiddleware(constants.ServiceTracker)(metrics.Wrap(middleware.CORS(trkRL.Wrap(stampTracker(mux)))))
	server := &http.Server{Addr: ":" + port, Handler: handler, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second}

	mode := "NATS JetStream"
	if bus == nil {
		mode = "HTTP bridge"
	}
	log.Info("tracker starting", "port", port, "mode", mode)
	lifecycle.ServeHTTP(lc, server, log, 30*time.Second)
}

// eventPublisher handles publishing to NATS or falling back to HTTP.
// Wraps the typed events.Publisher for events that live in pkg/events
// (Video / Audio / TrackerRejected) so those calls pick up the
// CurrentSchemaVersion default automatically. Analytics-mirror types
// (Impression / Click / Conversion / View) still go through the raw
// `bus` because they have their own SchemaVersion field set explicitly
// at the call site and need the HTTP fallback path when NATS is down.
type eventPublisher struct {
	bus          events.EventBus
	typed        *events.Publisher
	identityPub  *identityobserve.Publisher
	reportingURL string
	log          *slog.Logger
}

// publishAdvertiserIdentity links an advertiser's first-party visitor id to a
// hashed email seen on the same pixel, so the identity graph can later resolve
// that advertiser id to the publisher-side user who saw the ad (the bridge
// view-through attribution crosses). Fire-and-forget; needs BOTH ids (the
// identity-consumer only makes an edge from 2+ co-observed signals). Consent is
// the caller's gate.
func (p *eventPublisher) publishAdvertiserIdentity(traceID, advUID, hashedEmail, householdID string) {
	if p.identityPub == nil || advUID == "" || (hashedEmail == "" && householdID == "") {
		return
	}
	sigs := []identityobserve.Signal{{Value: advUID, Source: identity.SourceAdvertiserUserID}}
	if hashedEmail != "" {
		sigs = append(sigs, identityobserve.Signal{Value: hashedEmail, Source: identity.SourceHashedEmail})
	}
	// Household edge (uid ↔ hh:…), mirroring the SSP's serve-side identity
	// publish: the graph is what lets a purchase suppression EXPAND to the
	// buyer's household — for an anonymous guest the pixel is the only place
	// this edge can come from.
	if householdID != "" {
		sigs = append(sigs, identityobserve.Signal{Value: householdID, Source: identity.SourceHousehold})
	}
	p.identityPub.Publish(traceID, sigs, "")
}

// channelOrDefault maps the beacon's ch= param to a channel, defaulting to
// display when absent (display beacons don't bother setting it; video/native/
// audio beacons do, so their impressions are labelled correctly).
// stampTracker marks every /v1/t/* response with X-Adtech-Tracker so a beacon
// client can tell a real tracker answer from a transient tunnel/proxy 2xx.
// 2026-08-05 30-min soak: 492 impression beacons got a <300 status the tracker
// never served (localhost svclb path under load) — the simulator's delivery
// check trusted the bare status, so the loss was invisible client-side and
// VERIFY flagged it as pipeline slippage. The header is set on EVERY tracker
// response — including fraud/dedup silent-pixel paths — so it means "the
// tracker handled this", never "this was recorded" (a distinct recorded-marker
// would let bots detect fraud filtering by header diffing).
func stampTracker(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, routes.TrackerPrefix) {
			w.Header().Set("X-Adtech-Tracker", "1")
		}
		next.ServeHTTP(w, r)
	})
}

func channelOrDefault(ch string) string {
	if ch == "" {
		return constants.ChannelDisplay
	}
	return ch
}

// videoEventFromQuery / audioEventFromQuery build the typed media events from
// the ALREADY-PARSED beacon params. Attribution mirrors the impression pixel:
// cid/crid/pid/pubid plus advid → AccountID, so advertiser-tenant scoping on
// media_events works the same way it does on impressions — but ONLY when the
// URL signature verified (sigOK). With validation off the gate warn-and-allows
// unsigned beacons; recording them is fine, but trusting their attribution
// would let anyone forge quartiles into a victim tenant's completion metrics.
// Real traffic is always signed, so it keeps full attribution. Read-only over
// q — no lookups, no I/O (hot-path iron rule). Legacy beacons signed before
// advid landed simply yield empty attribution fields.
func videoEventFromQuery(q url.Values, traceID, eventType string, sigOK bool, now time.Time) events.VideoEvent {
	attrib := attribGate(q, sigOK)
	return events.VideoEvent{
		TraceID:     traceID,
		EventType:   eventType,
		CampaignID:  attrib("cid"),
		CreativeID:  attrib("crid"),
		PlacementID: attrib("pid"),
		PublisherID: attrib("pubid"),
		AccountID:   attrib("advid"),
		Timestamp:   now,
	}
}

func audioEventFromQuery(q url.Values, traceID, eventType string, sigOK bool, now time.Time) events.AudioEvent {
	attrib := attribGate(q, sigOK)
	return events.AudioEvent{
		TraceID:     traceID,
		EventType:   eventType,
		CampaignID:  attrib("cid"),
		CreativeID:  attrib("crid"),
		PlacementID: attrib("pid"),
		PublisherID: attrib("pubid"),
		AccountID:   attrib("advid"),
		Timestamp:   now,
	}
}

// attribGate returns a q.Get that yields "" for every key when the beacon's
// signature did not verify — unverified callers may be recorded (dev
// tolerance) but never get to assert who the event belongs to.
func attribGate(q url.Values, sigOK bool) func(string) string {
	if sigOK {
		return q.Get
	}
	return func(string) string { return "" }
}

func (p *eventPublisher) publishImpression(ctx context.Context, e analytics.ImpressionEvent, log *slog.Logger) {
	if p.publish(ctx, events.SubjectImpression, "imp:"+e.TraceID, e, log) {
		return
	}
	p.httpFallback(analytics.Event{Type: analytics.EventImpression, Impression: &e}, log)
}

func (p *eventPublisher) publishClick(ctx context.Context, e analytics.ClickEvent, log *slog.Logger) {
	if p.publish(ctx, events.SubjectClick, "click:"+e.TraceID, e, log) {
		return
	}
	p.httpFallback(analytics.Event{Type: analytics.EventClick, Click: &e}, log)
}

func (p *eventPublisher) publishView(ctx context.Context, e analytics.ViewEvent, log *slog.Logger) {
	if p.publish(ctx, events.SubjectView, "view:"+e.TraceID, e, log) {
		return
	}
}

func (p *eventPublisher) publishConversion(ctx context.Context, e analytics.ConversionEvent, log *slog.Logger) {
	if p.publish(ctx, events.SubjectConversion, "conv:"+e.TraceID+":"+e.ConversionType, e, log) {
		return
	}
	p.httpFallback(analytics.Event{Type: analytics.EventConversion, Conversion: &e}, log)
}

// publishVideo / publishAudio fire from the /v1/t/video and /v1/t/audio
// pixel handlers respectively. No HTTP fallback — engagement pings are
// observability, not billing source-of-truth, so a NATS outage just
// means the event is lost rather than triggering the standalone path.
// Routed via the typed publisher so SchemaVersion gets stamped.
func (p *eventPublisher) publishVideo(ctx context.Context, e events.VideoEvent, log *slog.Logger) {
	if err := p.typed.Video(ctx, e); err != nil {
		log.Warn("publish video failed", "error", err)
	}
}
func (p *eventPublisher) publishAudio(ctx context.Context, e events.AudioEvent, log *slog.Logger) {
	if err := p.typed.Audio(ctx, e); err != nil {
		log.Warn("publish audio failed", "error", err)
	}
}

// publishRejected fires whenever a pixel is dropped at the gate — HMAC
// strict-mode reject, fraud check blocked, or dedup hit. Fire-and-forget
// (no HTTP fallback): the rejection itself isn't billable, so reporting
// losing it isn't a billing-correctness issue, just an analytics gap.
// Called via `go p.publishRejected(...)` from inside the pixel handlers
// so the response path stays sub-10ms. Routed via the typed publisher
// so SchemaVersion gets stamped.
func (p *eventPublisher) publishRejected(ctx context.Context, eventType, reason, detail, traceID string, log *slog.Logger) {
	if err := p.typed.TrackerRejected(ctx, events.TrackerRejectedEvent{
		TraceID:   traceID,
		EventType: eventType,
		Reason:    reason,
		Detail:    detail,
		Timestamp: time.Now().UTC(),
	}); err != nil {
		log.Warn("publish rejected failed", "error", err)
	}
}

// firstNonEmpty returns the first non-empty string, or "".
func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// publishBehaviour emits one consent-gated interaction row for the profile
// store's behaviour_signals lake table. The uid param is ONLY baked into
// beacon URLs for consented serves (models.ServeRequest.BehaviourUserID), so
// its presence is the consent signal — no uid, no row. NATS-only,
// fire-and-forget: behaviour capture is profile enrichment, not billing.
func (p *eventPublisher) publishBehaviour(ctx context.Context, kind string, q url.Values, log *slog.Logger) {
	uid := q.Get("uid")
	if uid == "" {
		return
	}
	ev := events.BehaviourSignalEvent{
		SchemaVersion: events.CurrentSchemaVersion,
		// Prefer the beacon's tid; fall back to the request's OTel trace (set by
		// the tracker's HTTPMiddleware) so a retargeting pixel with no ?tid= (an
		// advertiser page tag) still carries a trace_id all the way downstream —
		// behaviour event → audience-rt → retargeting.enrolled → webhook.
		TraceID: firstNonEmpty(q.Get("tid"), tracing.TraceIDFromContext(ctx)),
		Kind:    kind,
		UserID:  uid,
		// The household (salted-IP hash) the serve resolved, when the beacon
		// carries it. Lets view-through fall back to a household match when the
		// exact user id doesn't line up (cross-device / CTV).
		HouseholdID: q.Get("hh"),
		PlacementID: q.Get("pid"),
		PublisherID: q.Get("pubid"),
		CampaignID:  q.Get("cid"),
		CreativeID:  q.Get("crid"),
		Channel:     channelOrDefault(q.Get("ch")),
		Geo:         q.Get("geo"),
		Device:      q.Get("dev"),
		// The advertiser account the row is scoped to. site_visit (retargeting
		// pixel) carries it as `aid`; impression/click/view beacons carry it as
		// `advid`. Stamping it on the ad-exposure kinds too is what lets
		// view-through attribution scope a click-less conversion's lookback to
		// the advertiser's own prior impressions.
		AccountID: firstNonEmpty(q.Get("aid"), q.Get("advid")),
		Tag:       q.Get("tag"),
		// DPA: the SKUs a shopper viewed/carted. Accept either a single sku= or
		// a skus= CSV; normalise to CSV. audience-rt records them per-user.
		SKUs:       firstNonEmpty(q.Get("skus"), q.Get("sku")),
		ObservedAt: time.Now().UTC(),
	}
	if err := p.typed.PublishJSON(ctx, events.SubjectBehaviourObserved, ev); err != nil {
		log.Warn("publish behaviour failed", "kind", kind, "error", err)
	}
}

// publish marshals and publishes to NATS with a stable message ID so
// JetStream drops republishes server-side (a publish-ack timeout makes the
// client resend; without the ID that's a SECOND stream entry and a
// double-counted, double-billed event — 251 of them in the 2026-07-18 hour
// run). msgID must be stable per logical event: subject + trace (+ any
// within-trace disambiguator). Returns true if successful.
func (p *eventPublisher) publish(ctx context.Context, subject, msgID string, payload interface{}, log *slog.Logger) bool {
	if p.bus == nil {
		return false
	}
	// Route through the SPOOL-ARMED typed publisher, never raw PublishDedup:
	// a failed publish is absorbed to disk and replayed with the SAME msgID
	// (dedup-safe). The old raw path surfaced the error and the HTTP fallback
	// below re-delivered the event out-of-band — when the "failed" publish
	// had actually reached the stream (ambiguous ack, standard during a NATS
	// bounce), that DOUBLE-DELIVERED: ~1k duplicate impressions per chaos
	// run, invisible to NATS-side dedup because HTTP bypasses it. The
	// fallback now exists only for bus==nil (no NATS configured at boot).
	if err := p.typed.PublishJSONID(ctx, subject, msgID, payload); err != nil {
		log.Warn("publish failed and spool unavailable", "subject", subject, "error", err)
		return false
	}
	return true
}

// isExpired returns true when the URL carries an exp=<unix-ts> param and
// the current time is past it. Empty / missing exp returns false (no
// expiry policy on this URL — the tracker accepts it). Unparseable exp
// also returns false: bad data is logged elsewhere as an invalid sig,
// not a freshness failure. Skew tolerance is +5s so a slightly fast
// client clock relative to the tracker doesn't reject borderline-fresh
// URLs.
// viewSigParams returns the query params that were part of the SIGNED
// viewability URL, i.e. all params except the client-measured dur/pct/area,
// which the browser appends after the URL is signed. Returns q unchanged when
// none are present (the common no-measurement case) to avoid an allocation.
func viewSigParams(q url.Values) url.Values {
	if !q.Has("dur") && !q.Has("pct") && !q.Has("area") {
		return q
	}
	out := make(url.Values, len(q))
	for k, v := range q {
		switch k {
		case "dur", "pct", "area":
			// measured client-side, appended post-signing — not in the signature
		default:
			out[k] = v
		}
	}
	return out
}

func isExpired(q url.Values, now time.Time) bool {
	raw := q.Get("exp")
	if raw == "" {
		return false
	}
	ts, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return false
	}
	return now.Unix() > ts+5
}

// appendTraceQuery appends adtech_tid=<traceID> to the redirect target so
// the landing page (our demo gateway pages, or anyone else who wants to
// stitch landing-side analytics to the click) can read the trace from
// the URL. Idempotent — if the target already carries adtech_tid we
// leave it alone. Empty traceID or redir → return redir unchanged.
func appendTraceQuery(redir, traceID string) string {
	if redir == "" || traceID == "" {
		return redir
	}
	if strings.Contains(redir, "adtech_tid=") {
		return redir
	}
	sep := "?"
	if strings.Contains(redir, "?") {
		sep = "&"
	}
	return redir + sep + "adtech_tid=" + url.QueryEscape(traceID)
}

func (p *eventPublisher) httpFallback(event analytics.Event, log *slog.Logger) {
	body, err := json.Marshal([]analytics.Event{event})
	if err != nil {
		log.Warn("marshal for http fallback failed", "error", err)
		return
	}
	resp, err := http.Post(p.reportingURL+routes.ReportingEvents, constants.ContentTypeJSON, bytes.NewReader(body))
	if err != nil {
		log.Warn("http fallback failed", "error", err)
		return
	}
	resp.Body.Close()
}
