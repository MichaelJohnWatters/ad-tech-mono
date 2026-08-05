package events

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"
)

// Publisher wraps an EventBus with typed publishing methods.
// All services use this instead of raw bus.Publish().
type Publisher struct {
	bus   EventBus
	log   *slog.Logger
	spool *Spool
}

// NewPublisher creates a typed event publisher.
func NewPublisher(bus EventBus, log *slog.Logger) *Publisher {
	return &Publisher{bus: bus, log: log}
}

// EnableSpool arms the disk spool: a failed publish is appended to disk
// instead of dropped, and a background drainer republishes once the bus
// answers again (replays dedupe via the spooled Nats-Msg-Id + reporting's
// business keys). Call once at service boot, before traffic. The returned
// stop function halts the drainer (graceful shutdown).
func (p *Publisher) EnableSpool(ctx context.Context, s *Spool) (stop func()) {
	p.spool = s
	dctx, cancel := context.WithCancel(ctx)
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-dctx.Done():
				return
			case <-t.C:
				n, err := s.drainBatch(dctx, 500, func(c context.Context, subject, msgID string, data []byte) error {
					return PublishDedup(c, p.bus, subject, msgID, data)
				})
				if n > 0 {
					p.log.Info("event spool drained", "events", n, "pressure_pct", s.Pressure())
				}
				if err != nil {
					p.log.Debug("event spool drain attempt failed (bus still down?)", "error", err)
				}
			}
		}
	}()
	return cancel
}

// Pressure reports the spool fill 0–100 (0 when no spool is armed). Feeds the
// front-door throttle: the exchange stamps it on auction responses and the
// SSP sheds serve requests as it rises.
func (p *Publisher) Pressure() int {
	if p.spool == nil {
		return 0
	}
	return p.spool.Pressure()
}

// PublishJSON marshals the payload and publishes to the given subject.
func (p *Publisher) PublishJSON(ctx context.Context, subject string, payload interface{}) error {
	return p.publishJSONID(ctx, subject, "", payload)
}

// PublishJSONID is PublishJSON with a stable dedup message ID — the public
// entry for callers that construct their own payload envelopes (the tracker's
// beacon path). Routing through here (not raw PublishDedup) matters: this is
// the spool-armed path, so a failed publish is absorbed to disk instead of
// surfacing an error that tempts callers into side-channel fallbacks (the
// tracker's old HTTP fallback double-delivered ~1k impressions per NATS
// bounce when the "failed" publish had actually reached the stream).
func (p *Publisher) PublishJSONID(ctx context.Context, subject, msgID string, payload interface{}) error {
	return p.publishJSONID(ctx, subject, msgID, payload)
}

// publishJSONID publishes with an optional stable message ID: JetStream
// drops republishes with the same ID inside the stream's Duplicates window,
// so a client resend after an ambiguous publish ack cannot double-enter the
// stream (the source of the hour-run's 251 duplicate impressions). Empty ID
// = plain publish (events with no one-per-trace identity).
func (p *Publisher) publishJSONID(ctx context.Context, subject, msgID string, payload interface{}) error {
	data, err := json.Marshal(payload)
	if err != nil {
		p.log.Error("failed to marshal event", "subject", subject, "error", err)
		return err
	}
	if err := PublishDedup(ctx, p.bus, subject, msgID, data); err != nil {
		// Publish failed (NATS down/stalled/deadline). With a spool armed the
		// event is preserved on disk and republished by the drainer — this
		// WAS a silent permanent loss (146,757 events in the 2026-08-05 VM
		// seizure). Absorbed = success from the caller's point of view.
		if p.spool != nil && p.spool.Append(subject, msgID, data) {
			p.log.Warn("publish failed — event spooled for replay", "subject", subject, "error", err, "pressure_pct", p.spool.Pressure())
			return nil
		}
		p.log.Error("failed to publish event", "subject", subject, "error", err)
		return err
	}
	p.log.Debug("event published", "subject", subject, "bytes", len(data))
	return nil
}

// Typed publish helpers for common events.
//
// Each method centralizes the SchemaVersion=1 default: callers that
// leave the field at its zero value get the current schema version
// stamped automatically. Bump CurrentSchemaVersion when wire format
// changes and every method here propagates the new value. Events
// passed by value, so the local mutation never escapes to the caller.

func (p *Publisher) AuctionWin(ctx context.Context, event AuctionWinEvent) error {
	if event.SchemaVersion == 0 {
		event.SchemaVersion = CurrentSchemaVersion
	}
	return p.publishJSONID(ctx, SubjectAuctionWin, "win:"+event.TraceID, event)
}

func (p *Publisher) AuctionComplete(ctx context.Context, event AuctionCompleteEvent) error {
	if event.SchemaVersion == 0 {
		event.SchemaVersion = CurrentSchemaVersion
	}
	return p.publishJSONID(ctx, SubjectAuctionComplete, "auction:"+event.TraceID, event)
}

func (p *Publisher) DSPCall(ctx context.Context, event DSPCallEvent) error {
	if event.SchemaVersion == 0 {
		event.SchemaVersion = CurrentSchemaVersion
	}
	return p.PublishJSON(ctx, SubjectDSPCall, event)
}

func (p *Publisher) BudgetDepleted(ctx context.Context, event BudgetDepletedEvent) error {
	if event.SchemaVersion == 0 {
		event.SchemaVersion = CurrentSchemaVersion
	}
	return p.PublishJSON(ctx, SubjectBudgetDepleted, event)
}

func (p *Publisher) BalanceDepleted(ctx context.Context, event BalanceDepletedEvent) error {
	if event.SchemaVersion == 0 {
		event.SchemaVersion = CurrentSchemaVersion
	}
	return p.PublishJSON(ctx, SubjectBalanceDepleted, event)
}

func (p *Publisher) CampaignSpendSnapshot(ctx context.Context, event CampaignSpendSnapshotEvent) error {
	if event.SchemaVersion == 0 {
		event.SchemaVersion = CurrentSchemaVersion
	}
	return p.PublishJSON(ctx, SubjectCampaignSpendSnapshot, event)
}

func (p *Publisher) CampaignStateChanged(ctx context.Context, event CampaignStateEvent) error {
	if event.SchemaVersion == 0 {
		event.SchemaVersion = CurrentSchemaVersion
	}
	return p.PublishJSON(ctx, SubjectCampaignStateChanged, event)
}

func (p *Publisher) OptOut(ctx context.Context, event OptOutEvent) error {
	if event.SchemaVersion == 0 {
		event.SchemaVersion = CurrentSchemaVersion
	}
	return p.PublishJSON(ctx, SubjectPrivacyOptOut, event)
}

func (p *Publisher) CacheInvalidate(ctx context.Context, subject string, event CacheInvalidateEvent) error {
	if event.SchemaVersion == 0 {
		event.SchemaVersion = CurrentSchemaVersion
	}
	return p.PublishJSON(ctx, subject, event)
}

func (p *Publisher) DirectWin(ctx context.Context, event DirectWinEvent) error {
	if event.SchemaVersion == 0 {
		event.SchemaVersion = CurrentSchemaVersion
	}
	return p.publishJSONID(ctx, SubjectDirectWin, "directwin:"+event.TraceID, event)
}

func (p *Publisher) PrebidOutboundWin(ctx context.Context, event PrebidOutboundWinEvent) error {
	if event.SchemaVersion == 0 {
		event.SchemaVersion = CurrentSchemaVersion
	}
	return p.publishJSONID(ctx, SubjectPrebidOutboundWin, "preout:"+event.TraceID, event)
}

func (p *Publisher) Video(ctx context.Context, event VideoEvent) error {
	if event.SchemaVersion == 0 {
		event.SchemaVersion = CurrentSchemaVersion
	}
	return p.PublishJSON(ctx, SubjectVideo, event)
}

func (p *Publisher) Audio(ctx context.Context, event AudioEvent) error {
	if event.SchemaVersion == 0 {
		event.SchemaVersion = CurrentSchemaVersion
	}
	return p.PublishJSON(ctx, SubjectAudio, event)
}

func (p *Publisher) ServeNoFill(ctx context.Context, event ServeNoFillEvent) error {
	if event.SchemaVersion == 0 {
		event.SchemaVersion = CurrentSchemaVersion
	}
	return p.PublishJSON(ctx, SubjectServeNoFill, event)
}

func (p *Publisher) TrackerRejected(ctx context.Context, event TrackerRejectedEvent) error {
	if event.SchemaVersion == 0 {
		event.SchemaVersion = CurrentSchemaVersion
	}
	return p.PublishJSON(ctx, SubjectTrackerRejected, event)
}

func (p *Publisher) AdserverRenderFailed(ctx context.Context, event AdserverRenderFailedEvent) error {
	if event.SchemaVersion == 0 {
		event.SchemaVersion = CurrentSchemaVersion
	}
	return p.PublishJSON(ctx, SubjectAdserverRenderFailed, event)
}

func (p *Publisher) AdserverFreqCapBlocked(ctx context.Context, event AdserverFreqCapBlockedEvent) error {
	if event.SchemaVersion == 0 {
		event.SchemaVersion = CurrentSchemaVersion
	}
	return p.PublishJSON(ctx, SubjectAdserverFreqCapBlocked, event)
}
