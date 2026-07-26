// datafee.go — data-monetization accrual (ADR 0009 phase 1).
//
// The SSP publishes a DataFeeEvent when an EXTERNAL bidder wins an auction
// whose request carried fee-bearing audience data. Nothing is owed until the
// impression is DELIVERED (billing-on-impression convention), so the event
// parks in data_fee_pending until the impression for its trace arrives, then
// accrues:
//
//	debit  extseat:{seat}:payable            fee            (receivable)
//	credit advertiser:{owner}:balance        fee − margin   (+ prepay upsert)
//	credit platform:datafee_margin           margin
//
// plus one data_fee_earnings row per segment for the owner's portal surface.
//
// The pending join is DURABLE (Postgres): reporting replicas partition the
// event stream, so the fee event and the impression routinely land on
// different pods — an in-memory join would silently drop fees at >1
// replica. DELETE ... RETURNING claims the row atomically, so exactly one
// pod accrues even if impressions were somehow redelivered across pods.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
)

// pendingTTL bounds how long an un-impressed win's attribution survives —
// generous next to the tracker URL expiry; a win that never serves inside it
// earns nothing, by design.
const pendingTTL = 24 * time.Hour

type dataFeeAccrual struct {
	db *sql.DB
	// marginPctFn reads reporting.data_fee_margin_pct live per accrual.
	marginPctFn func() float64
	log         *slog.Logger
}

func newDataFeeAccrual(db *sql.DB, marginPctFn func() float64, log *slog.Logger) *dataFeeAccrual {
	if db == nil {
		return nil
	}
	return &dataFeeAccrual{db: db, marginPctFn: marginPctFn, log: log}
}

// HandleEvent parks one DataFeeEvent until its impression arrives.
// Idempotent per trace (PK) — NATS redelivery is a no-op.
func (a *dataFeeAccrual) HandleEvent(ctx context.Context, msg *events.Message) error {
	if a == nil {
		return msg.Ack()
	}
	var ev events.DataFeeEvent
	if err := json.Unmarshal(msg.Data, &ev); err != nil {
		a.log.Error("data-fee event decode failed", "error", err)
		return msg.Ack() // don't redeliver bad data
	}
	if ev.TraceID == "" || len(ev.Segments) == 0 {
		return msg.Ack()
	}
	if _, err := a.db.ExecContext(ctx, `
INSERT INTO data_fee_pending (trace_id, payload) VALUES ($1, $2)
ON CONFLICT (trace_id) DO NOTHING`, ev.TraceID, msg.Data); err != nil {
		a.log.Error("data-fee pending insert failed", "trace_id", ev.TraceID, "error", err)
		return msg.Nak()
	}
	// Opportunistic expiry sweep — rides the (rare) fee-event write, keeps
	// the table from accumulating never-impressed wins.
	if _, err := a.db.ExecContext(ctx,
		`DELETE FROM data_fee_pending WHERE created_at < now() - $1::interval`,
		pendingTTL.String()); err != nil {
		a.log.Warn("data-fee pending sweep failed", "error", err)
	}
	return msg.Ack()
}

// AccrueOnImpression settles the trace's parked attribution, if any. Called
// on EVERY impression — a single PK lookup; almost always a fast miss.
// Failures log at ERROR and return (the impression itself is already
// recorded and billed; fee accrual must never Nak the impression).
func (a *dataFeeAccrual) AccrueOnImpression(ctx context.Context, traceID string) {
	if a == nil || traceID == "" {
		return
	}
	// Cheap non-transactional probe first: ~every impression is a miss, and
	// paying BEGIN/ROLLBACK round-trips per impression on a small pool just
	// to discover that is real load. The transactional DELETE below re-checks,
	// so a concurrent claim between probe and claim stays exactly-once.
	var one int
	err := a.db.QueryRowContext(ctx,
		`SELECT 1 FROM data_fee_pending WHERE trace_id = $1`, traceID).Scan(&one)
	if err == sql.ErrNoRows {
		return
	}
	if err != nil {
		a.log.Error("data-fee pending probe failed", "trace_id", traceID, "error", err)
		return
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		a.log.Error("data-fee accrual begin failed", "trace_id", traceID, "error", err)
		return
	}
	defer tx.Rollback()
	// Data-fee settlement is inherently cross-tenant — one event credits multiple
	// data-owner accounts, debits the external seat, and books the platform
	// margin — so it runs under the platform hatch (security #77). The
	// tenant_isolation policies are USING-only, so platform_read admits the writes.
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.platform_read', 'on', true)`); err != nil {
		a.log.Error("data-fee accrual platform-read set failed", "trace_id", traceID, "error", err)
		return
	}

	var payload []byte
	err = tx.QueryRowContext(ctx,
		`DELETE FROM data_fee_pending WHERE trace_id = $1 RETURNING payload`,
		traceID).Scan(&payload)
	if err == sql.ErrNoRows {
		return // the common case: no fee-bearing data rode this trace
	}
	if err != nil {
		a.log.Error("data-fee pending claim failed", "trace_id", traceID, "error", err)
		return
	}
	var ev events.DataFeeEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		a.log.Error("data-fee pending payload corrupt (dropping)", "trace_id", traceID, "error", err)
		_ = tx.Commit() // consume the corrupt row rather than retrying forever
		return
	}

	marginPct := a.marginPctFn()
	if marginPct < 0 {
		marginPct = 0
	}
	if marginPct > 100 {
		marginPct = 100
	}
	for _, seg := range ev.Segments {
		// Per-impression micro-dollars: the fee is a CPM.
		feeMicros := seg.FeeMicros / 1000
		if feeMicros <= 0 {
			continue
		}
		marginMicros := int64(float64(feeMicros) * marginPct / 100)
		ownerNetMicros := feeMicros - marginMicros
		if _, err := tx.ExecContext(ctx, `
INSERT INTO data_fee_earnings
  (trace_id, segment_id, account_id, winner_seat, publisher_id, placement_id,
   fee_micros, owner_net_micros, margin_micros)
VALUES ($1, $2, $3::uuid, $4, $5, $6, $7, $8, $9)
ON CONFLICT (trace_id, segment_id) DO NOTHING`,
			ev.TraceID, seg.SegmentID, seg.OwnerAccountID, ev.WinnerSeat,
			ev.PublisherID, ev.PlacementID, feeMicros, ownerNetMicros, marginMicros); err != nil {
			a.log.Error("data-fee earnings insert failed", "trace_id", traceID, "segment", seg.SegmentID, "error", err)
			return
		}
		// Double-entry in dollars (the ledger_entries convention): the
		// external seat owes the full fee; the owner is credited net, the
		// platform keeps the margin. reference_id = trace so an auditor can
		// walk fee → impression → auction.
		feeUSD := float64(feeMicros) / 1e6
		netUSD := float64(ownerNetMicros) / 1e6
		marginUSD := float64(marginMicros) / 1e6
		if _, err := tx.ExecContext(ctx, `
INSERT INTO ledger_entries (account_code, entry_type, amount, currency, reference_type, reference_id)
VALUES ('extseat:' || $1 || ':payable', 'debit',  $2, 'USD', 'data_fee', $5),
       ('advertiser:' || $3 || ':balance', 'credit', $4, 'USD', 'data_fee', $5),
       ('platform:datafee_margin', 'credit', $6, 'USD', 'data_fee', $5)`,
			ev.WinnerSeat, feeUSD, seg.OwnerAccountID, netUSD, ev.TraceID+"/"+seg.SegmentID, marginUSD); err != nil {
			a.log.Error("data-fee ledger insert failed", "trace_id", traceID, "segment", seg.SegmentID, "error", err)
			return
		}
		// Real money: the owner's prepay balance grows by the net fee.
		if _, err := tx.ExecContext(ctx, `
INSERT INTO advertiser_balances (account_id, balance, currency, updated_at)
VALUES ($1::uuid, $2, 'USD', now())
ON CONFLICT (account_id) DO UPDATE
  SET balance = advertiser_balances.balance + $2, updated_at = now()`,
			seg.OwnerAccountID, netUSD); err != nil {
			a.log.Error("data-fee balance credit failed", "trace_id", traceID, "owner", seg.OwnerAccountID, "error", err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		a.log.Error("data-fee accrual commit failed", "trace_id", traceID, "error", err)
		return
	}
	a.log.Info("data fee accrued", "trace_id", traceID,
		"winner_seat", ev.WinnerSeat, "segments", len(ev.Segments),
		"margin_pct", fmt.Sprintf("%.1f", marginPct))
}
