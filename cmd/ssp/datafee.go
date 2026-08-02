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
	log *slog.Logger
}

// newDataFeePublisher wraps the bus; nil bus → nil publisher (no-op).
func newDataFeePublisher(bus events.EventBus, log *slog.Logger) *dataFeePublisher {
	if bus == nil {
		return nil
	}
	return &dataFeePublisher{pub: events.NewPublisher(bus, log), log: log}
}

// Observe publishes the attribution record for one auction, fire-and-forget.
// No-ops on: nil receiver, no fee-bearing stamped segments, no winner, or an
// INTERNAL winner (internal cross-tenant data fees are a deliberate non-goal for
// now — they'd have to interact with budget gating).
//
// The billable seat is the exchange's TRUSTED SettlementSeat — bound to which
// configured endpoint won — NOT bidResp.SeatBid[0].Seat, which the bidder
// self-declares and could set to dodge (empty/UUID) or misdirect (a competitor's
// id) the receivable. Empty SettlementSeat = internal or non-exchange caller →
// skip. See docs/datafee-seat-integrity-plan.md.
func (p *dataFeePublisher) Observe(r *http.Request, traceID string, pl postgres.PlacementRow, bidResp openrtb.BidResponse, feeSegs []events.DataFeeSegment) {
	if p == nil || len(feeSegs) == 0 || bidResp.NoBid {
		return
	}
	if len(bidResp.SeatBid) == 0 || len(bidResp.SeatBid[0].Bid) == 0 {
		return
	}
	seat := bidResp.SettlementSeat
	if seat == "" {
		return // internal winner (our own demand) — out of scope
	}
	// A bidder that self-declares a seat different from the trusted one is either
	// misconfigured or attempting to dodge/misdirect the fee. Bill the trusted
	// seat regardless; surface the mismatch so ops can see the attempt.
	if declared := bidResp.SeatBid[0].Seat; declared != "" && declared != seat {
		p.log.Warn("data-fee seat mismatch: billing the trusted seat, not the self-declared one",
			"trace_id", traceID, "declared_seat", declared, "trusted_seat", seat)
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
