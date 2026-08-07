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
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
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

// batchKeep pairs a surviving message with its decoded event and dedup keys.
type batchKeep[T any] struct {
	msg      *events.Message
	event    *T
	dedupKey string // "" when the message had no stable id (dedup skipped)
	bizKey   string // "" when the subject has no one-per-trace business key
}

// batchProcess is the shared pipeline for every core subject. decode turns a
// payload into a typed event (returns false to drop poison data); insert bulk-
// writes the survivors; after runs a per-event side-effect (billing) that must
// happen only once the rows are durable and before ack. after may be nil.
// bizKey (nil = skip) derives a BUSINESS identity for the second dedup
// layer: the message-id layer catches JetStream REdeliveries (same stream
// sequence), but a publisher republish after an ambiguous ack is a NEW
// sequence carrying the same logical event — only a content-derived key
// (trace id + within-trace disambiguator) can catch it. Belt to the
// publish-side Nats-Msg-Id suspenders (the server window is finite).
func batchProcess[T any](
	ctx context.Context,
	c *EventConsumer,
	msgs []*events.Message,
	decode func(data []byte) (*T, bool),
	bizKey func(*T) string,
	insert func(ctx context.Context, es []*T) error,
	afterBatch func(ctx context.Context, es []*T),
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
		var bkey string
		if bizKey != nil {
			if k := bizKey(e); k != "" {
				bkey = "dedup:biz:" + m.Subject + ":" + k
				isNew, err := c.dedup.MarkProcessed(ctx, bkey, c.dedupTTL)
				if err != nil {
					if key != "" {
						_ = c.dedup.UnmarkProcessed(ctx, key)
					}
					m.Nak() // Redis blip — fail closed, same as above
					continue
				}
				if !isNew {
					m.Ack() // publisher republish (new sequence, same event) — drop
					continue
				}
			}
		}
		keep = append(keep, batchKeep[T]{msg: m, event: e, dedupKey: key, bizKey: bkey})
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
			if k.bizKey != "" {
				_ = c.dedup.UnmarkProcessed(ctx, k.bizKey)
			}
			k.msg.Nak()
		}
		return err
	}

	// 2) bill the whole batch in ONE call (the scale lever: N events → ~1
	//    ledger + ~1 balance round-trip), then 3) ack. Billing is best-effort
	//    relative to the ack gate, exactly as in the single-message handlers.
	if afterBatch != nil {
		afterBatch(ctx, es)
	}
	for _, k := range keep {
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
		func(e *analytics.ImpressionEvent) string { return e.TraceID },
		c.batch.InsertImpressions,
		func(ctx context.Context, es []*analytics.ImpressionEvent) {
			// Data monetization rides the batch path too — same per-trace
			// settle as the per-message handler (fast PK misses).
			for _, e := range es {
				c.dataFee.AccrueOnImpression(ctx, e.TraceID)
			}
			if c.billing == nil {
				return
			}
			spends := make([]billing.SpendEvent, len(es))
			for i, e := range es {
				spends[i] = billing.SpendEvent{
					TraceID: e.TraceID, CampaignID: e.CampaignID, CreativeID: e.CreativeID,
					PlacementID: e.PlacementID, PublisherID: e.PublisherID, AdvertiserID: e.AccountID,
					ClearingPrice: e.ClearingPriceUSD, Currency: spendCurrency(e.ClearingCurrency),
					BidModel: billing.BidModel(e.BidModel), DealType: c.resolveDealType(ctx, e.DealID),
					EventType: "impression", Timestamp: e.Timestamp,
				}
			}
			c.billing.ProcessBatch(ctx, spends)
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
		func(e *analytics.ClickEvent) string { return e.TraceID },
		c.batch.InsertClicks,
		func(ctx context.Context, es []*analytics.ClickEvent) {
			if c.billing == nil {
				return
			}
			// Batch the CPC settles into one ledger + balance + pacing round-trip
			// (per-trace reservation lookup/dedup is unchanged inside the batch).
			reqs := make([]billing.SettleRequest, 0, len(es))
			for _, e := range es {
				reqs = append(reqs, billing.SettleRequest{TraceID: e.TraceID, EventType: "click"})
			}
			if _, err := c.billing.ProcessSettleBatch(ctx, reqs); err != nil {
				c.log.Error("batch click settle failed", "count", len(reqs), "error", err)
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
			// Attribute in place before the row is inserted/settled — the same
			// *event pointer flows to both the insert and the settle below, so the
			// stored row records the linkage and the settle credits the exposure.
			// Click-through (ctid) is already stamped; this adds view-through.
			c.attributeConversion(ctx, &e)
			return &e, true
		},
		func(e *analytics.ConversionEvent) string { return e.TraceID + ":" + e.ConversionType },
		c.batch.InsertConversions,
		func(ctx context.Context, es []*analytics.ConversionEvent) {
			if c.billing == nil || !c.attributionEnabled() {
				return
			}
			// Attribution closes the loop: settle against the EARNING exposure's
			// trace (SettleTraceID = AttributedTraceID when resolved), not the
			// conversion's own (reservation-less) synthetic order trace.
			reqs := make([]billing.SettleRequest, 0, len(es))
			for _, e := range es {
				reqs = append(reqs, billing.SettleRequest{TraceID: e.SettleTraceID(), EventType: "conversion"})
			}
			if _, err := c.billing.ProcessSettleBatch(ctx, reqs); err != nil {
				c.log.Error("batch conversion settle failed", "count", len(reqs), "error", err)
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
		func(e *analytics.ViewEvent) string { return e.TraceID },
		c.batch.InsertViews,
		func(ctx context.Context, es []*analytics.ViewEvent) {
			if c.billing == nil {
				return
			}
			// vCPM settles only on a viewable impression (see handleView), so only
			// those traces enter the batch.
			reqs := make([]billing.SettleRequest, 0, len(es))
			for _, e := range es {
				if !e.IABViewable {
					continue
				}
				reqs = append(reqs, billing.SettleRequest{TraceID: e.TraceID, EventType: "viewable"})
			}
			if len(reqs) > 0 {
				if _, err := c.billing.ProcessSettleBatch(ctx, reqs); err != nil {
					c.log.Error("batch view settle failed", "count", len(reqs), "error", err)
				}
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
		func(e *analytics.AuctionEvent) string { return e.TraceID },
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
		func(e *analytics.AuctionWinEvent) string { return e.TraceID },
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
		func(e *analytics.AuctionWinEvent) string { return e.TraceID },
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
		func(e *analytics.AuctionWinEvent) string { return e.TraceID },
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
				PositionMs: src.PositionMs, CampaignID: src.CampaignID,
				CreativeID: src.CreativeID, PlacementID: src.PlacementID,
				PublisherID: src.PublisherID, AccountID: src.AccountID,
				Timestamp: src.Timestamp,
			}, true
		},
		nil, // audio quartiles: several legitimate events per trace
		c.batch.InsertMediaEvents,
		nil,
	)
}

func (c *EventConsumer) handleDSPCallBatch(ctx context.Context, msgs []*events.Message) error {
	return batchProcess(ctx, c, msgs,
		func(data []byte) (*analytics.DSPCallEvent, bool) {
			var src events.DSPCallEvent
			if err := json.Unmarshal(data, &src); err != nil {
				c.log.Error("batch: decode dsp_call", "error", err)
				return nil, false
			}
			if src.Timestamp.IsZero() {
				src.Timestamp = time.Now()
			}
			return &analytics.DSPCallEvent{
				TraceID: src.TraceID, AuctionID: src.AuctionID, Channel: src.Channel,
				DSPEndpoint: src.DSPEndpoint, BidReceived: src.BidReceived, BidPriceUSD: src.BidPriceUSD,
				LatencyMs: src.LatencyMs, TimedOut: src.TimedOut, NoBidReason: src.NoBidReason,
				SchemaVersion: 1, Timestamp: src.Timestamp,
			}, true
		},
		nil, // one call per DSP per trace: multiple legitimate rows
		c.batch.InsertDSPCalls,
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
				PositionMs: src.PositionMs, CampaignID: src.CampaignID,
				CreativeID: src.CreativeID, PlacementID: src.PlacementID,
				PublisherID: src.PublisherID, AccountID: src.AccountID,
				Timestamp: src.Timestamp,
			}, true
		},
		nil, // media quartiles: several legitimate events per trace
		c.batch.InsertMediaEvents,
		nil,
	)
}

// handleBehaviourSignalBatch decodes consent-gated behavioural observations and
// bulk-inserts them into ClickHouse (ADR 0006 phase 1) — landed ALONGSIDE the
// pipeline's lake dual-write, which is untouched. No business dedup key: a
// single trace legitimately produces several behaviour rows (request +
// impression + click + …), so dedup rides only on the JetStream message id.
func (c *EventConsumer) handleBehaviourSignalBatch(ctx context.Context, msgs []*events.Message) error {
	return batchProcess(ctx, c, msgs,
		func(data []byte) (*analytics.BehaviourSignalRow, bool) {
			var src events.BehaviourSignalEvent
			if err := json.Unmarshal(data, &src); err != nil {
				c.log.Error("batch: decode behaviour signal", "error", err)
				return nil, false
			}
			if src.ObservedAt.IsZero() {
				src.ObservedAt = time.Now()
			}
			return &analytics.BehaviourSignalRow{
				TraceID: src.TraceID, Kind: src.Kind, UserID: src.UserID, HouseholdID: src.HouseholdID,
				PlacementID: src.PlacementID, PublisherID: src.PublisherID, CampaignID: src.CampaignID,
				CreativeID: src.CreativeID, Channel: src.Channel, Categories: src.Categories,
				Geo: src.Geo, Device: src.Device, AccountID: src.AccountID, Tag: src.Tag,
				ObservedAt: src.ObservedAt,
			}, true
		},
		nil, // several legitimate behaviour rows per trace — no one-per-trace key
		c.batch.InsertBehaviourSignals,
		nil,
	)
}

// handleProfileSignalBatch decodes onboarding-signal BATCHES and EXPANDS each
// event's IDs into one analytics.ProfileSignalRow per id — exactly the
// expansion the pipeline lake sink does (profileSignalRecord), so both stores
// hold the same one-row-per-id shape (ADR 0006 phase 1). Because one NATS
// message fans out to N rows, this can't use the generic 1-msg-1-row decoder;
// the survivors decode to *ProfileSignalEvent and the insert closure collects
// every batch's ids into one bulk InsertProfileSignals call.
//
// Dedup rides on the message id only (marked/acked per message by batchProcess);
// there's no per-row business key, so a message is all-or-nothing, matching the
// lake sink's at-least-once semantics (duplicate appended rows on redelivery,
// never lost ones — the profile-builder reconcile dedupes on read).
func (c *EventConsumer) handleProfileSignalBatch(ctx context.Context, msgs []*events.Message) error {
	return batchProcess(ctx, c, msgs,
		func(data []byte) (*events.ProfileSignalEvent, bool) {
			var src events.ProfileSignalEvent
			if err := json.Unmarshal(data, &src); err != nil {
				c.log.Error("batch: decode profile signal", "error", err)
				return nil, false
			}
			if src.ObservedAt.IsZero() {
				src.ObservedAt = time.Now()
			}
			return &src, true
		},
		nil, // batch messages carry many ids — no one-per-trace key
		func(ctx context.Context, evs []*events.ProfileSignalEvent) error {
			var rows []*analytics.ProfileSignalRow
			for _, ev := range evs {
				for _, id := range ev.IDs {
					rows = append(rows, &analytics.ProfileSignalRow{
						TraceID: ev.TraceID, IngestTraceID: ev.IngestTraceID, AccountID: ev.AccountID, Provider: ev.Provider,
						ProviderID: ev.ProviderID, DataParty: ev.DataParty,
						Source: ev.Source, Access: ev.Access, SegmentID: ev.SegmentID,
						SegmentName: ev.SegmentName, Visibility: ev.Visibility, Consent: ev.Consent,
						IDType: id.IDType, IDValue: id.IDValue, ObservedAt: ev.ObservedAt,
					})
				}
			}
			return c.batch.InsertProfileSignals(ctx, rows)
		},
		nil,
	)
}

// handleBehaviourSignal is the per-message fallback for the behaviour-observed
// subject, used when the batch consumer is disabled. It decodes one event and
// inserts it as a 1-row batch (the store's bulk InsertBehaviourSignals is the
// only write path for these ADR-0006 tables). Bad data is acked, not
// redelivered forever; an insert failure naks for redelivery.
func (c *EventConsumer) handleBehaviourSignal(ctx context.Context, msg *events.Message) error {
	bi, ok := c.store.(analytics.BatchInserter)
	if !ok {
		return msg.Ack() // backend can't hold these tables — drop, don't wedge the stream
	}
	var src events.BehaviourSignalEvent
	if err := json.Unmarshal(msg.Data, &src); err != nil {
		c.log.Error("failed to decode behaviour signal", "error", err)
		return msg.Ack()
	}
	if src.ObservedAt.IsZero() {
		src.ObservedAt = time.Now()
	}
	row := &analytics.BehaviourSignalRow{
		TraceID: src.TraceID, Kind: src.Kind, UserID: src.UserID, HouseholdID: src.HouseholdID,
		PlacementID: src.PlacementID, PublisherID: src.PublisherID, CampaignID: src.CampaignID,
		CreativeID: src.CreativeID, Channel: src.Channel, Categories: src.Categories,
		Geo: src.Geo, Device: src.Device, AccountID: src.AccountID, Tag: src.Tag,
		ObservedAt: src.ObservedAt,
	}
	if err := bi.InsertBehaviourSignals(ctx, []*analytics.BehaviourSignalRow{row}); err != nil {
		c.log.Error("failed to write behaviour signal", "error", err, "trace_id", src.TraceID)
		return msg.Nak()
	}
	return msg.Ack()
}

// handleProfileSignal is the per-message fallback for the profile-signal
// subject, used when the batch consumer is disabled. It EXPANDS one batch's ids
// into one row per id (mirroring the lake sink) and bulk-inserts them.
func (c *EventConsumer) handleProfileSignal(ctx context.Context, msg *events.Message) error {
	bi, ok := c.store.(analytics.BatchInserter)
	if !ok {
		return msg.Ack()
	}
	var ev events.ProfileSignalEvent
	if err := json.Unmarshal(msg.Data, &ev); err != nil {
		c.log.Error("failed to decode profile signal", "error", err)
		return msg.Ack()
	}
	if ev.ObservedAt.IsZero() {
		ev.ObservedAt = time.Now()
	}
	if len(ev.IDs) == 0 {
		return msg.Ack()
	}
	rows := make([]*analytics.ProfileSignalRow, 0, len(ev.IDs))
	for _, id := range ev.IDs {
		rows = append(rows, &analytics.ProfileSignalRow{
			TraceID: ev.TraceID, IngestTraceID: ev.IngestTraceID, AccountID: ev.AccountID, Provider: ev.Provider,
			ProviderID: ev.ProviderID, DataParty: ev.DataParty,
			Source: ev.Source, Access: ev.Access, SegmentID: ev.SegmentID,
			SegmentName: ev.SegmentName, Visibility: ev.Visibility, Consent: ev.Consent,
			IDType: id.IDType, IDValue: id.IDValue, ObservedAt: ev.ObservedAt,
		})
	}
	if err := bi.InsertProfileSignals(ctx, rows); err != nil {
		c.log.Error("failed to write profile signals", "error", err, "trace_id", ev.TraceID)
		return msg.Nak()
	}
	return msg.Ack()
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
		events.SubjectDSPCall:           c.handleDSPCallBatch,
		// Profile-store tables (ADR 0006 phase 1): reporting consumes these off
		// its OWN group (NATSGroupReporting, subscribed via SubscribeBatch in
		// main.go) so BOTH reporting→ClickHouse and pipeline→lake receive every
		// message. The pipeline's lake sink is untouched.
		events.SubjectBehaviourObserved: c.handleBehaviourSignalBatch,
		events.SubjectProfileSignal:     c.handleProfileSignalBatch,
	}
}

// connectReportingRedis returns a real Redis L2 cache if reachable, else an
// in-memory L2 (fail-open — batching + dedup still work in a dev env without
// Redis, just without cross-pod dedup). Mirrors cmd/tracker's connectRedis.
func connectReportingRedis(cfg *config.Config, log *slog.Logger) cache.L2Cache {
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
