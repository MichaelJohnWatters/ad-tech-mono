package events

import (
	"context"
	"encoding/json"
	"log/slog"
)

// Publisher wraps an EventBus with typed publishing methods.
// All services use this instead of raw bus.Publish().
type Publisher struct {
	bus EventBus
	log *slog.Logger
}

// NewPublisher creates a typed event publisher.
func NewPublisher(bus EventBus, log *slog.Logger) *Publisher {
	return &Publisher{bus: bus, log: log}
}

// PublishJSON marshals the payload and publishes to the given subject.
func (p *Publisher) PublishJSON(ctx context.Context, subject string, payload interface{}) error {
	return p.publishJSONID(ctx, subject, "", payload)
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
