// datafee.go — data-monetization attribution (ADR 0009 phase 1).
//
// When an EXTERNAL bidder wins an auction whose bid request carried
// fee-bearing audience data (public, taxonomy-labelled segments stamped as
// user.data — consent already gated at the stamp), the SSP publishes one
// DataFeeEvent naming the segments, their owners, and their fees. Only the
// SSP can do this: attribution must never ride the bid request itself, which
// external parties receive verbatim. Reporting holds the event until the
// impression for the trace arrives, then accrues (billing-on-impression
// convention — a win that never serves earns nothing).
package main

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
)

type dataFeePublisher struct {
	pub *events.Publisher
}

// newDataFeePublisher wraps the bus; nil bus → nil publisher (no-op).
func newDataFeePublisher(bus events.EventBus, log *slog.Logger) *dataFeePublisher {
	if bus == nil {
		return nil
	}
	return &dataFeePublisher{pub: events.NewPublisher(bus, log)}
}

// Observe publishes the attribution record for one auction, fire-and-forget.
// No-ops on: nil receiver, no fee-bearing stamped segments, no winner, or an
// INTERNAL winner (internal seats are tenant account UUIDs; internal
// cross-tenant data fees are a deliberate non-goal for now — they'd have to
// interact with budget gating).
func (p *dataFeePublisher) Observe(r *http.Request, traceID string, pl postgres.PlacementRow, bidResp openrtb.BidResponse, feeSegs []events.DataFeeSegment) {
	if p == nil || len(feeSegs) == 0 || bidResp.NoBid {
		return
	}
	if len(bidResp.SeatBid) == 0 || len(bidResp.SeatBid[0].Bid) == 0 {
		return
	}
	seat := bidResp.SeatBid[0].Seat
	if seat == "" || uuidPattern.MatchString(seat) {
		return // internal winner (tenant account UUID) — out of scope
	}
	ev := events.DataFeeEvent{
		SchemaVersion: events.CurrentSchemaVersion,
		TraceID:       traceID,
		PlacementID:   pl.ID,
		PublisherID:   pl.PublisherID,
		WinnerSeat:    seat,
		Segments:      feeSegs,
		ClearingPrice: bidResp.SeatBid[0].Bid[0].Price,
		Timestamp:     time.Now().UTC(),
	}
	go func() {
		_ = p.pub.PublishJSON(context.WithoutCancel(r.Context()), events.SubjectDataFee, ev)
	}()
}
