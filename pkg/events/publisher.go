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
	data, err := json.Marshal(payload)
	if err != nil {
		p.log.Error("failed to marshal event", "subject", subject, "error", err)
		return err
	}
	if err := p.bus.Publish(ctx, subject, data); err != nil {
		p.log.Error("failed to publish event", "subject", subject, "error", err)
		return err
	}
	p.log.Debug("event published", "subject", subject, "bytes", len(data))
	return nil
}

// Typed publish helpers for common events

func (p *Publisher) AuctionWin(ctx context.Context, event AuctionWinEvent) error {
	return p.PublishJSON(ctx, SubjectAuctionWin, event)
}

func (p *Publisher) AuctionComplete(ctx context.Context, event AuctionCompleteEvent) error {
	return p.PublishJSON(ctx, SubjectAuctionComplete, event)
}

func (p *Publisher) BudgetDepleted(ctx context.Context, event BudgetDepletedEvent) error {
	return p.PublishJSON(ctx, SubjectBudgetDepleted, event)
}

func (p *Publisher) CampaignStateChanged(ctx context.Context, event CampaignStateEvent) error {
	return p.PublishJSON(ctx, SubjectCampaignStateChanged, event)
}

func (p *Publisher) OptOut(ctx context.Context, event OptOutEvent) error {
	return p.PublishJSON(ctx, SubjectPrivacyOptOut, event)
}

func (p *Publisher) CacheInvalidate(ctx context.Context, subject string, event CacheInvalidateEvent) error {
	return p.PublishJSON(ctx, subject, event)
}

func (p *Publisher) DirectWin(ctx context.Context, event DirectWinEvent) error {
	return p.PublishJSON(ctx, SubjectDirectWin, event)
}

func (p *Publisher) PrebidOutboundWin(ctx context.Context, event PrebidOutboundWinEvent) error {
	return p.PublishJSON(ctx, SubjectPrebidOutboundWin, event)
}

func (p *Publisher) Video(ctx context.Context, event VideoEvent) error {
	return p.PublishJSON(ctx, SubjectVideo, event)
}

func (p *Publisher) Audio(ctx context.Context, event AudioEvent) error {
	return p.PublishJSON(ctx, SubjectAudio, event)
}

func (p *Publisher) ServeNoFill(ctx context.Context, event ServeNoFillEvent) error {
	return p.PublishJSON(ctx, SubjectServeNoFill, event)
}

func (p *Publisher) TrackerRejected(ctx context.Context, event TrackerRejectedEvent) error {
	return p.PublishJSON(ctx, SubjectTrackerRejected, event)
}

func (p *Publisher) AdserverRenderFailed(ctx context.Context, event AdserverRenderFailedEvent) error {
	return p.PublishJSON(ctx, SubjectAdserverRenderFailed, event)
}
