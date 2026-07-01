package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/billing"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache"
	cacheredis "github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache/redis"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
)

// batch.go — the NATS batch-consumer path for the high-volume core events.
//
// A JetStream fetch already returns N messages; this path inserts them as one
// atomic ClickHouse block (BatchInserter) instead of N single-row inserts (the
// "too many parts" anti-pattern). It is opt-in via
// reporting.clickhouse_batch_consumer and only engages when the analytics
// backend implements analytics.BatchInserter and a dedup store is wired.
//
// Durability invariant, identical to the single-message path but applied to N:
//
//	insert (durable, atomic) → bill → ack
//
// Nothing is acked before its row is durably in the store; an insert failure
// naks the whole batch (NATS redelivers, bounded by MaxDeliver→DLQ) and bills
// nothing. Per-message dedup (Redis SetNX on the stream sequence) makes
// redelivery safe — duplicates are skipped, not double-counted or double-billed.

// batchKeep pairs a surviving message with its decoded event and dedup key.
type batchKeep[T any] struct {
	msg      *events.Message
	event    *T
	dedupKey string // "" when the message had no stable id (dedup skipped)
}

// batchProcess is the shared pipeline for every core subject. decode turns a
// payload into a typed event (returns false to drop poison data); insert bulk-
// writes the survivors; after runs a per-event side-effect (billing) that must
// happen only once the rows are durable and before ack. after may be nil.
func batchProcess[T any](
	ctx context.Context,
	c *EventConsumer,
	msgs []*events.Message,
	decode func(data []byte) (*T, bool),
	insert func(ctx context.Context, es []*T) error,
	after func(ctx context.Context, e *T),
) error {
	keep := make([]batchKeep[T], 0, len(msgs))
	for _, m := range msgs {
		// Dedup on the message's stable id (stream sequence). Marking before
		// processing + unmarking on insert failure mirrors IdempotentHandler.
		var key string
		isNew := true
		if m.MessageID != "" {
			key = "dedup:" + m.Subject + ":" + m.MessageID
			var err error
			isNew, err = c.dedup.MarkProcessed(ctx, key, c.dedupTTL)
			if err != nil {
				// Can't guarantee dedup — nak, retry later rather than risk a
				// double-count. (Redis blip; fail-closed here on purpose.)
				m.Nak()
				continue
			}
		}
		if !isNew {
			m.Ack() // duplicate redelivery — already processed, drop it
			continue
		}
		e, ok := decode(m.Data)
		if !ok {
			m.Ack() // poison payload — drop, never redeliver
			continue
		}
		keep = append(keep, batchKeep[T]{msg: m, event: e, dedupKey: key})
	}
	if len(keep) == 0 {
		return nil
	}

	es := make([]*T, len(keep))
	for i := range keep {
		es[i] = keep[i].event
	}

	// 1) DURABLE WRITE FIRST — one atomic block. On failure: bill nothing,
	//    roll back dedup marks so redelivery reprocesses, nak the whole batch.
	if err := insert(ctx, es); err != nil {
		c.log.Error("batch insert failed, naking batch", "subject", msgs[0].Subject, "n", len(es), "error", err)
		for _, k := range keep {
			if k.dedupKey != "" {
				_ = c.dedup.UnmarkProcessed(ctx, k.dedupKey)
			}
			k.msg.Nak()
		}
		return err
	}

	// 2) bill each survivor, then 3) ack. Billing is best-effort relative to
	//    the ack gate, exactly as in the single-message handlers.
	for _, k := range keep {
		if after != nil {
			after(ctx, k.event)
		}
		k.msg.Ack()
	}
	return nil
}

func (c *EventConsumer) handleImpressionBatch(ctx context.Context, msgs []*events.Message) error {
	return batchProcess(ctx, c, msgs,
		func(data []byte) (*analytics.ImpressionEvent, bool) {
			var e analytics.ImpressionEvent
			if err := json.Unmarshal(data, &e); err != nil {
				c.log.Error("batch: decode impression", "error", err)
				return nil, false
			}
			if e.Timestamp.IsZero() {
				e.Timestamp = time.Now()
			}
			if e.SchemaVersion == 0 {
				e.SchemaVersion = 1
			}
			return &e, true
		},
		c.batch.InsertImpressions,
		func(ctx context.Context, e *analytics.ImpressionEvent) {
			if c.billing == nil {
				return
			}
			c.billing.ProcessEvent(ctx, billing.SpendEvent{
				TraceID: e.TraceID, CampaignID: e.CampaignID, CreativeID: e.CreativeID,
				PlacementID: e.PlacementID, PublisherID: e.PublisherID, AdvertiserID: e.AccountID,
				ClearingPrice: e.ClearingPriceUSD, Currency: "USD",
				BidModel: billing.BidModel(e.BidModel), DealType: e.DealID,
				EventType: "impression", Timestamp: e.Timestamp,
			})
		},
	)
}

func (c *EventConsumer) handleClickBatch(ctx context.Context, msgs []*events.Message) error {
	return batchProcess(ctx, c, msgs,
		func(data []byte) (*analytics.ClickEvent, bool) {
			var e analytics.ClickEvent
			if err := json.Unmarshal(data, &e); err != nil {
				c.log.Error("batch: decode click", "error", err)
				return nil, false
			}
			if e.Timestamp.IsZero() {
				e.Timestamp = time.Now()
			}
			if e.SchemaVersion == 0 {
				e.SchemaVersion = 1
			}
			return &e, true
		},
		c.batch.InsertClicks,
		func(ctx context.Context, e *analytics.ClickEvent) {
			if c.billing == nil {
				return
			}
			if _, err := c.billing.SettleByTrace(ctx, e.TraceID, "click"); err != nil {
				c.log.Warn("batch click settle failed", "trace_id", e.TraceID, "error", err)
			}
		},
	)
}

func (c *EventConsumer) handleConversionBatch(ctx context.Context, msgs []*events.Message) error {
	return batchProcess(ctx, c, msgs,
		func(data []byte) (*analytics.ConversionEvent, bool) {
			var e analytics.ConversionEvent
			if err := json.Unmarshal(data, &e); err != nil {
				c.log.Error("batch: decode conversion", "error", err)
				return nil, false
			}
			if e.Timestamp.IsZero() {
				e.Timestamp = time.Now()
			}
			if e.SchemaVersion == 0 {
				e.SchemaVersion = 1
			}
			return &e, true
		},
		c.batch.InsertConversions,
		func(ctx context.Context, e *analytics.ConversionEvent) {
			if c.billing == nil {
				return
			}
			if _, err := c.billing.SettleByTrace(ctx, e.TraceID, "conversion"); err != nil {
				c.log.Warn("batch conversion settle failed", "trace_id", e.TraceID, "error", err)
			}
		},
	)
}

func (c *EventConsumer) handleViewBatch(ctx context.Context, msgs []*events.Message) error {
	return batchProcess(ctx, c, msgs,
		func(data []byte) (*analytics.ViewEvent, bool) {
			var e analytics.ViewEvent
			if err := json.Unmarshal(data, &e); err != nil {
				c.log.Error("batch: decode view", "error", err)
				return nil, false
			}
			if e.Timestamp.IsZero() {
				e.Timestamp = time.Now()
			}
			if e.SchemaVersion == 0 {
				e.SchemaVersion = 1
			}
			return &e, true
		},
		c.batch.InsertViews,
		func(ctx context.Context, e *analytics.ViewEvent) {
			// vCPM settles only on a viewable impression (see handleView).
			if c.billing == nil || !e.IABViewable {
				return
			}
			if _, err := c.billing.SettleByTrace(ctx, e.TraceID, "viewable"); err != nil {
				c.log.Warn("batch view settle failed", "trace_id", e.TraceID, "error", err)
			}
		},
	)
}

func (c *EventConsumer) handleAuctionBatch(ctx context.Context, msgs []*events.Message) error {
	return batchProcess(ctx, c, msgs,
		func(data []byte) (*analytics.AuctionEvent, bool) {
			var e analytics.AuctionEvent
			if err := json.Unmarshal(data, &e); err != nil {
				c.log.Error("batch: decode auction", "error", err)
				return nil, false
			}
			if e.Timestamp.IsZero() {
				e.Timestamp = time.Now()
			}
			if e.SchemaVersion == 0 {
				e.SchemaVersion = 1
			}
			return &e, true
		},
		c.batch.InsertAuctions,
		nil,
	)
}

func (c *EventConsumer) handleAuctionWinBatch(ctx context.Context, msgs []*events.Message) error {
	return batchProcess(ctx, c, msgs,
		func(data []byte) (*analytics.AuctionWinEvent, bool) {
			var src events.AuctionWinEvent
			if err := json.Unmarshal(data, &src); err != nil {
				c.log.Error("batch: decode auction win", "error", err)
				return nil, false
			}
			if src.Timestamp.IsZero() {
				src.Timestamp = time.Now()
			}
			return &analytics.AuctionWinEvent{
				TraceID: src.TraceID, AuctionID: src.AuctionID, WinnerDSP: src.WinnerDSP,
				CampaignID: src.CampaignID, CreativeID: src.CreativeID, PlacementID: src.PlacementID,
				PublisherID: src.PublisherID, AdvertiserID: src.AdvertiserID, ClearingPrice: src.ClearingPrice,
				Currency: src.Currency, BidModel: src.BidModel, DealID: src.DealID, Channel: src.Channel,
				SchemaVersion: 1, Timestamp: src.Timestamp,
			}, true
		},
		c.batch.InsertAuctionWins,
		nil,
	)
}

func (c *EventConsumer) handleDirectWinBatch(ctx context.Context, msgs []*events.Message) error {
	return batchProcess(ctx, c, msgs,
		func(data []byte) (*analytics.AuctionWinEvent, bool) {
			var src events.DirectWinEvent
			if err := json.Unmarshal(data, &src); err != nil {
				c.log.Error("batch: decode direct win", "error", err)
				return nil, false
			}
			if src.Timestamp.IsZero() {
				src.Timestamp = time.Now()
			}
			return &analytics.AuctionWinEvent{
				TraceID: src.TraceID, AuctionID: src.TraceID, WinnerDSP: "publisher-adserver",
				CampaignID: src.PublisherLineItemID, CreativeID: src.CreativeID, PlacementID: src.PlacementID,
				PublisherID: src.PublisherID, AdvertiserID: src.DemandSource, ClearingPrice: src.CPM,
				Currency: src.Currency, BidModel: "direct:" + src.PriorityTier, Channel: "display",
				SchemaVersion: 1, Timestamp: src.Timestamp,
			}, true
		},
		c.batch.InsertAuctionWins,
		nil,
	)
}

func (c *EventConsumer) handlePrebidOutboundWinBatch(ctx context.Context, msgs []*events.Message) error {
	return batchProcess(ctx, c, msgs,
		func(data []byte) (*analytics.AuctionWinEvent, bool) {
			var src events.PrebidOutboundWinEvent
			if err := json.Unmarshal(data, &src); err != nil {
				c.log.Error("batch: decode prebid outbound win", "error", err)
				return nil, false
			}
			if src.Timestamp.IsZero() {
				src.Timestamp = time.Now()
			}
			return &analytics.AuctionWinEvent{
				TraceID: src.TraceID, AuctionID: src.TraceID, WinnerDSP: src.PrebidEndpoint,
				PlacementID: src.PlacementID, PublisherID: src.PublisherID, AdvertiserID: src.Seat,
				ClearingPrice: src.ClearingPrice, Currency: src.Currency, BidModel: "prebid_outbound",
				DealID: src.DealID, Channel: "display", SchemaVersion: 1, Timestamp: src.Timestamp,
			}, true
		},
		c.batch.InsertAuctionWins,
		nil,
	)
}

func (c *EventConsumer) handleVideoBatch(ctx context.Context, msgs []*events.Message) error {
	return batchProcess(ctx, c, msgs,
		func(data []byte) (*analytics.MediaEvent, bool) {
			var src events.VideoEvent
			if err := json.Unmarshal(data, &src); err != nil {
				c.log.Error("batch: decode video", "error", err)
				return nil, false
			}
			if src.Timestamp.IsZero() {
				src.Timestamp = time.Now()
			}
			return &analytics.MediaEvent{
				TraceID: src.TraceID, Channel: "video", EventType: src.EventType,
				PositionMs: src.PositionMs, Timestamp: src.Timestamp,
			}, true
		},
		c.batch.InsertMediaEvents,
		nil,
	)
}

func (c *EventConsumer) handleAudioBatch(ctx context.Context, msgs []*events.Message) error {
	return batchProcess(ctx, c, msgs,
		func(data []byte) (*analytics.MediaEvent, bool) {
			var src events.AudioEvent
			if err := json.Unmarshal(data, &src); err != nil {
				c.log.Error("batch: decode audio", "error", err)
				return nil, false
			}
			if src.Timestamp.IsZero() {
				src.Timestamp = time.Now()
			}
			return &analytics.MediaEvent{
				TraceID: src.TraceID, Channel: "audio", EventType: src.EventType,
				PositionMs: src.PositionMs, Timestamp: src.Timestamp,
			}, true
		},
		c.batch.InsertMediaEvents,
		nil,
	)
}

// coreBatchHandlers returns the subject→batch-handler map for the high-volume
// core events that go through the bulk-insert path when batching is enabled.
func (c *EventConsumer) coreBatchHandlers() map[string]events.BatchHandler {
	return map[string]events.BatchHandler{
		events.SubjectImpression:        c.handleImpressionBatch,
		events.SubjectClick:             c.handleClickBatch,
		events.SubjectConversion:        c.handleConversionBatch,
		events.SubjectView:              c.handleViewBatch,
		events.SubjectAuctionComplete:   c.handleAuctionBatch,
		events.SubjectAuctionWin:        c.handleAuctionWinBatch,
		events.SubjectDirectWin:         c.handleDirectWinBatch,
		events.SubjectPrebidOutboundWin: c.handlePrebidOutboundWinBatch,
		events.SubjectVideo:             c.handleVideoBatch,
		events.SubjectAudio:             c.handleAudioBatch,
	}
}

// connectReportingRedis returns a real Redis L2 cache if reachable, else an
// in-memory L2 (fail-open — batching + dedup still work in a dev env without
// Redis, just without cross-pod dedup). Mirrors cmd/tracker's connectRedis.
func connectReportingRedis(cfg *config.Config, log *slog.Logger) cache.L2Cache {
	addr := cfg.Get("redis.url", routes.DefaultRedisAddr)
	pwd := cfg.Get("redis.password", "")
	db := cfg.GetInt("redis.db", 0)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, err := cacheredis.New(ctx, cacheredis.Config{Addr: addr, Password: pwd, DB: db})
	if err != nil {
		log.Warn("reporting: redis unreachable, dedup uses in-memory L2 (single-pod only)", "addr", addr, "error", err)
		return cache.NewMemoryL2()
	}
	log.Info("reporting: redis connected for batch dedup", "addr", addr)
	return client
}
