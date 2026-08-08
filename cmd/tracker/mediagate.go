package main

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adserving"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/fraud"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
)

// mediaEventGate runs the SAME internet-facing checks as the impression handler
// — HMAC signature → URL expiry → real-time fraud → dedup — on a /v1/t/video or
// /v1/t/audio beacon before it's recorded. Without it those endpoints
// over-counted (hls.js/dash.js prefetch + seek-back re-fires every quartile,
// recorded each time) and were spoofable (GET /v1/t/video?event=complete with no
// signature). The two validation toggles are functions so they stay live-tunable
// via the config manager, exactly like the impression path.
type mediaEventGate struct {
	sigKeys       func() []string // overlap set: config key + non-revoked hmac_tracker secrets
	sigValidation func() bool     // tracker.signature_validation (default false)
	expValidation func() bool     // tracker.exp_validation (default true)
	fraud         *fraud.RealTimeChecker
	dedup         *Dedup
	publisher     *eventPublisher
}

// allow runs the gate and returns (record, sigOK). record is true when the
// event should be recorded. sigOK is the best-effort signature result
// REGARDLESS of the enforcement toggle: with validation off a bad/missing sig
// still records (dev tolerance), but the caller must blank the beacon's
// attribution params — otherwise anyone could forge cid/advid/pubid straight
// into a victim tenant's media reports. On rejection allow writes the response
// itself — 403 for a strict-mode bad signature, 410 for an expired URL, and a
// silent 204 for fraud/dedup (so the player keeps playing and detection isn't
// revealed) — and records a TrackerRejected event.
//
// dedup is namespaced per quartile per trace via eventType+":"+event (e.g.
// "video:start"), so distinct quartiles on one trace each record once while a
// re-fired quartile is dropped. Paired with the SSAI per-pod-ad trace that makes
// the key effectively per-ad-per-quartile.
func (g mediaEventGate) allow(w http.ResponseWriter, r *http.Request, eventType, event, traceID string, reqLog *slog.Logger) (bool, bool) {
	q := r.URL.Query()
	ctx := logger.WithTraceID(r.Context(), traceID)

	sigOK := adserving.ValidateSignatureAny(r.URL.Path, q, g.sigKeys())
	if !sigOK {
		reqLog.Warn("invalid signature", "path", r.URL.Path)
		if g.sigValidation() {
			go g.publisher.publishRejected(context.WithoutCancel(ctx),
				eventType, "invalid_signature", event, traceID, reqLog)
			http.Error(w, "invalid signature", http.StatusForbidden)
			return false, false
		}
	}

	if g.expValidation() && isExpired(q, time.Now()) {
		reqLog.Warn("expired url", "path", r.URL.Path, "exp", q.Get("exp"))
		go g.publisher.publishRejected(context.WithoutCancel(ctx),
			eventType, "expired", q.Get("exp"), traceID, reqLog)
		http.Error(w, "url expired", http.StatusGone)
		return false, sigOK
	}

	fraudResult := g.fraud.Check(fraud.Request{
		IP: clientIP(r), UserAgent: r.UserAgent(),
		TraceID: traceID, Referer: r.Referer(),
	})
	if fraudResult.Blocked {
		reqLog.Warn("media event fraud blocked", "event_type", eventType,
			"score", fraudResult.Score, "reasons", fraudResult.Reasons)
		go g.publisher.publishRejected(context.WithoutCancel(ctx),
			eventType, "fraud", strings.Join(fraudResult.Reasons, ","), traceID, reqLog)
		w.WriteHeader(http.StatusNoContent) // silent — don't reveal detection
		return false, sigOK
	}

	// Per-quartile-per-trace dedup: distinct quartiles on one trace each record
	// once; a re-fired quartile (prefetch / seek-back) is dropped.
	if !g.dedup.FirstSeen(ctx, eventType+":"+event, traceID) {
		reqLog.Debug("duplicate media event, dropping", "trace_id", traceID, "event", event)
		go g.publisher.publishRejected(context.WithoutCancel(ctx),
			eventType, "dedup", event, traceID, reqLog)
		w.WriteHeader(http.StatusNoContent)
		return false, sigOK
	}
	return true, sigOK
}
