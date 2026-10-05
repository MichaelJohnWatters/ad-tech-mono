// burl.go — OpenRTB billing-notice (burl) firing at the billable moment.
//
// The buyer's burl rides the AuctionWinEvent (BillingURL, auction macros
// already substituted by the exchange at win time) but must not fire until
// the impression actually BOOKS — OpenRTB 2.5+'s billable event, distinct
// from the win notice which fires whether or not the ad ever renders.
//
// Reporting replicas partition the event stream (the win and its impression
// routinely land on different pods), so the join is DURABLE (burl_pending,
// same pattern as data_fee_pending): the win parks the URL; the impression
// claims it with DELETE ... RETURNING (exactly one pod fires, redeliveries
// are no-ops) and GETs it fire-and-forget. Purely a notice — failures log
// and are dropped; the money truth is the billing engine, never this call.
package main

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"time"
)

// burlPendingTTL bounds how long an un-impressed win's billing notice
// survives — a win that never serves inside it is never billable, by design.
const burlPendingTTL = 24 * time.Hour

type burlNotifier struct {
	db   *sql.DB
	http *http.Client
	log  *slog.Logger
}

func newBurlNotifier(db *sql.DB, log *slog.Logger) *burlNotifier {
	if db == nil {
		return nil
	}
	return &burlNotifier{db: db, http: &http.Client{Timeout: 3 * time.Second}, log: log}
}

// ParkFromWin stores the win's billing URL until its impression arrives.
// Idempotent per trace (PK) — NATS redelivery is a no-op. Nil-tolerant.
func (b *burlNotifier) ParkFromWin(ctx context.Context, traceID, billingURL string) {
	if b == nil || traceID == "" || billingURL == "" {
		return
	}
	if _, err := b.db.ExecContext(ctx, `
INSERT INTO burl_pending (trace_id, billing_url) VALUES ($1, $2)
ON CONFLICT (trace_id) DO NOTHING`, traceID, billingURL); err != nil {
		b.log.Error("burl pending insert failed", "trace_id", traceID, "error", err)
		return
	}
	// Opportunistic expiry sweep rides the (comparatively rare) win write.
	if _, err := b.db.ExecContext(ctx,
		`DELETE FROM burl_pending WHERE created_at < now() - $1::interval`,
		burlPendingTTL.String()); err != nil {
		b.log.Warn("burl pending sweep failed", "error", err)
	}
}

// FireOnImpression claims and fires the trace's parked billing notice, if
// any. Called on EVERY impression — a single PK probe, almost always a fast
// miss. The claim (DELETE ... RETURNING) is what makes the fire exactly-once
// across replicas; the GET itself is async fire-and-forget. Never blocks or
// fails the impression (already recorded and billed).
func (b *burlNotifier) FireOnImpression(ctx context.Context, traceID string) {
	if b == nil || traceID == "" {
		return
	}
	// Cheap probe first — the transactional-free DELETE below re-checks, so
	// a concurrent claim between probe and claim stays exactly-once.
	var one int
	err := b.db.QueryRowContext(ctx,
		`SELECT 1 FROM burl_pending WHERE trace_id = $1`, traceID).Scan(&one)
	if err == sql.ErrNoRows {
		return
	}
	if err != nil {
		b.log.Error("burl pending probe failed", "trace_id", traceID, "error", err)
		return
	}
	var billingURL string
	err = b.db.QueryRowContext(ctx,
		`DELETE FROM burl_pending WHERE trace_id = $1 RETURNING billing_url`, traceID).Scan(&billingURL)
	if err == sql.ErrNoRows {
		return // another replica claimed it
	}
	if err != nil {
		b.log.Error("burl pending claim failed", "trace_id", traceID, "error", err)
		return
	}
	go func() {
		req, err := http.NewRequest(http.MethodGet, billingURL, nil)
		if err != nil {
			b.log.Warn("burl build request failed", "trace_id", traceID, "error", err)
			return
		}
		resp, err := b.http.Do(req)
		if err != nil {
			b.log.Warn("burl fire failed", "trace_id", traceID, "error", err)
			return
		}
		resp.Body.Close()
		b.log.Debug("billing notice fired", "trace_id", traceID, "status", resp.StatusCode)
	}()
}
