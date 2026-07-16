package main

// behaviour.go — consent-gated behavioural signal capture (profile store).
//
// One request-level row per consented ad request, published fire-and-forget
// to adtech.behaviour.observed and landed in the behaviour_signals Delta
// table by the pipeline. Rows are SELF-CONTAINED: the placement's content
// categories are stamped here, at event time, from the SSP's placement warm
// cache — a behavioural rule evaluated months later must not depend on what
// the placement's categories are *then*.
//
// The consent gate is local and strict: privacy.Evaluate over the request's
// own regulatory signals (the same params applyPrivacySignals forwards
// downstream) must return Personalise=true, else nothing is published. This
// differs from segment stamping (where the DSP enforces the gate before
// USE): a behaviour row is retained data, not a transient bid annotation, so
// the gate applies at capture.

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/privacy"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
)

type behaviourPublisher struct {
	pub *events.Publisher
}

// newBehaviourPublisher wraps the bus; nil bus → nil publisher (no-op).
func newBehaviourPublisher(bus events.EventBus, log *slog.Logger) *behaviourPublisher {
	if bus == nil {
		return nil
	}
	return &behaviourPublisher{pub: events.NewPublisher(bus, log)}
}

// Observe publishes the request-level behaviour row. Fire-and-forget; safe
// on a nil receiver. Skipped when there is no user key at all or when the
// request's regulatory signals deny personalisation.
func (p *behaviourPublisher) Observe(r *http.Request, traceID, userKey, householdID string, pl postgres.PlacementRow, channel, geo, device string) {
	if p == nil || (userKey == "" && householdID == "") {
		return
	}
	if !requestConsent(r).Personalise {
		return
	}
	if channel == "" {
		channel = "display"
	}
	ev := events.BehaviourSignalEvent{
		SchemaVersion: events.CurrentSchemaVersion,
		TraceID:       traceID,
		Kind:          "request",
		UserID:        userKey,
		HouseholdID:   householdID,
		PlacementID:   pl.ID,
		PublisherID:   pl.PublisherID,
		Channel:       channel,
		Categories:    strings.Join(pl.Categories, ","),
		Geo:           geo,
		Device:        device,
		ObservedAt:    time.Now().UTC(),
	}
	go func() {
		_ = p.pub.PublishJSON(context.WithoutCancel(r.Context()), events.SubjectBehaviourObserved, ev)
	}()
}

// behaviourUserKey returns the serve request's user key (user_id, else UID2)
// when its regulatory signals permit personalisation, else "" — the value of
// models.ServeRequest.BehaviourUserID, which the ad server bakes into
// tracker beacons so interaction events can feed behaviour_signals.
func behaviourUserKey(r *http.Request) string {
	if !requestConsent(r).Personalise {
		return ""
	}
	if uid := r.URL.Query().Get("user_id"); uid != "" {
		return uid
	}
	return r.URL.Query().Get("uid2")
}

// requestConsent evaluates the ad-tag request's own regulatory signals —
// the query params / headers applyPrivacySignals forwards on the bid
// request. The SSP has no opt-out-registry access, so Level stays LevelNone;
// level-2/3 users are separately blocked at the DSP and level-3 deletion
// purges the lake.
func requestConsent(r *http.Request) privacy.Decision {
	q := r.URL.Query()
	return privacy.Evaluate(privacy.SignalsFromQuery(q.Get, r.Header.Get("Sec-GPC")))
}
