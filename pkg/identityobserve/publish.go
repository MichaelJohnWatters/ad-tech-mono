package identityobserve

import (
	"context"
	"log/slog"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/identity"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
)

// Publisher publishes ObservedEvents to NATS, where the identity-consumer builds
// graph edges. Shared by every service that sees identity signals (the SSP from
// its ad-tag requests, the exchange from inbound Prebid requests, …) so they all
// feed the one consumer. A nil *Publisher is a no-op.
type Publisher struct {
	bus events.EventBus
	log *slog.Logger
}

// NewPublisher returns a Publisher, or nil when there's no bus (which callers
// can pass straight through — Publish is nil-safe).
func NewPublisher(bus events.EventBus, log *slog.Logger) *Publisher {
	if bus == nil {
		return nil
	}
	return &Publisher{bus: bus, log: log}
}

// Publish emits an observation, best-effort. Skips when there's nothing to link
// (fewer than 2 ids and no fingerprint). Fire-and-forget on an independent
// context so a finished request doesn't cancel the publish.
func (p *Publisher) Publish(traceID string, ids []Signal, fingerprint string) {
	if p == nil {
		return
	}
	if len(ids) < 2 && !(len(ids) >= 1 && fingerprint != "") {
		return
	}
	payload, err := Marshal(ObservedEvent{TraceID: traceID, IDs: ids, Fingerprint: fingerprint})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.bus.Publish(ctx, events.SubjectIdentityObserved, payload); err != nil {
		p.log.Debug("identity observation publish failed (best-effort)", "error", err)
	}
}

// PublishRequest extracts the identity signals from an OpenRTB bid request and
// publishes them. For services that hold a parsed bid request (the exchange's
// Prebid path) rather than an HTTP request.
func (p *Publisher) PublishRequest(traceID string, req *openrtb.BidRequest) {
	ids, fp := SignalsFromRequest(req)
	p.Publish(traceID, ids, fp)
}

// SignalsFromRequest pulls the identifiers (and IP+UA fingerprint) out of an
// OpenRTB bid request's User + Device, in a fixed order, de-duplicated.
func SignalsFromRequest(req *openrtb.BidRequest) ([]Signal, string) {
	if req == nil {
		return nil, ""
	}
	var ids []Signal
	seen := map[string]bool{}
	add := func(v, s string) {
		if v == "" || seen[v] {
			return
		}
		seen[v] = true
		ids = append(ids, Signal{Value: v, Source: s})
	}
	if u := req.User; u != nil {
		add(u.ID, identity.SourcePublisherUserID)
		add(openrtb.UID2From(u), identity.SourceUID2)
		if u.Ext != nil {
			add(u.Ext.HashedEmail, identity.SourceHashedEmail)
			add(u.Ext.PublisherUserID, identity.SourcePublisherUserID)
		}
	}
	var fp string
	if d := req.Device; d != nil {
		add(d.IFA, identity.SourceDeviceID)
		if d.IP != "" && d.UA != "" {
			fp = d.IP + "|" + d.UA
		}
	}
	return ids, fp
}
