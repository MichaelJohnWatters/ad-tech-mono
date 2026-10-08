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
//
// PERF (handoff 08): the burl lifecycle was ~40% of sampled Postgres time
// at 150rps — a per-win INSERT with the 24h expiry sweep riding EVERY win
// write, plus a probe+claim pair per impression. Now: parks buffer through
// a single flusher goroutine (multi-row unnest INSERT, flush at
// parkBatchMax rows or parkFlushInterval), the sweep runs on its own
// burlSweepInterval ticker, and the impression claim is the single
// DELETE ... RETURNING (a zero-row DELETE costs an index probe, no WAL, no
// dead tuples — the separate SELECT probe bought nothing).
//
// Batching caveat (accepted, documented): a park now lands ≤parkFlushInterval
// after the win event. If the trace's impression is consumed BEFORE the park
// lands, the burl never fires (parked too late, swept at TTL). That race
// pre-exists at NATS cross-stream ordering (impression event consumed before
// the win event) — batching widens it by ≤25ms, the same posture as the
// event spool's loss window. The money truth is unaffected either way.
package main

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/lib/pq"
)

// burlPendingTTL bounds how long an un-impressed win's billing notice
// survives — a win that never serves inside it is never billable, by design.
const burlPendingTTL = 24 * time.Hour

const (
	// parkFlushInterval is the max time a park waits in the buffer — also the
	// added width of the park-after-impression race documented above.
	parkFlushInterval = 25 * time.Millisecond
	// parkBatchMax flushes early when the buffer fills this many rows.
	parkBatchMax = 256
	// parkBufferCap bounds the enqueue channel; a full buffer falls back to
	// a direct single-row insert rather than blocking the event consumer.
	parkBufferCap = 4096
	// burlSweepInterval is the expiry-sweep cadence. Was riding every win
	// write (~140 range DELETEs/sec at 150rps); once a minute is plenty for
	// a 24h TTL.
	burlSweepInterval = time.Minute
)

type parkedBurl struct {
	traceID    string
	billingURL string
}

type burlNotifier struct {
	db   *sql.DB
	http *http.Client
	log  *slog.Logger

	parked   chan parkedBurl
	stopped  chan struct{}
	done     chan struct{}
	stopOnce sync.Once
	// flushFn is the batch writer (flushParked in production; a seam for
	// tests, which exercise the batching loop without a database).
	flushFn func(rows []parkedBurl)
}

func newBurlNotifier(db *sql.DB, log *slog.Logger) *burlNotifier {
	if db == nil {
		return nil
	}
	b := &burlNotifier{
		db:      db,
		http:    &http.Client{Timeout: 3 * time.Second},
		log:     log,
		parked:  make(chan parkedBurl, parkBufferCap),
		stopped: make(chan struct{}),
		done:    make(chan struct{}),
	}
	b.flushFn = b.flushParked
	go b.run()
	return b
}

// Stop flushes any buffered parks and halts the flusher. Safe to call once
// at shutdown (lifecycle-wired in main); nil-tolerant like the hooks.
func (b *burlNotifier) Stop() {
	if b == nil {
		return
	}
	b.stopOnce.Do(func() {
		close(b.stopped)
		<-b.done
	})
}

// ParkFromWin stores the win's billing URL until its impression arrives.
// Idempotent per trace (PK) — NATS redelivery is a no-op. Nil-tolerant.
// Enqueues onto the flusher (≤parkFlushInterval to durability); a full
// buffer or shutdown degrades to today's direct single-row insert.
func (b *burlNotifier) ParkFromWin(ctx context.Context, traceID, billingURL string) {
	if b == nil || traceID == "" || billingURL == "" {
		return
	}
	p := parkedBurl{traceID: traceID, billingURL: billingURL}
	// Stopped must be checked BEFORE offering the send: the buffered channel
	// stays sendable after shutdown (nobody drains it), and a select with
	// both cases ready picks randomly — a post-Stop park could strand in the
	// channel. (Caught by TestBurlStopFlushesBuffer.)
	select {
	case <-b.stopped:
		b.flushFn([]parkedBurl{p})
		return
	default:
	}
	select {
	case b.parked <- p:
	default: // buffer full — degrade to a direct insert, never block the consumer
		b.flushFn([]parkedBurl{p})
	}
}

// run is the single flusher: batches parks (size- or time-triggered) and
// owns the expiry sweep ticker.
func (b *burlNotifier) run() {
	defer close(b.done)
	sweep := time.NewTicker(burlSweepInterval)
	defer sweep.Stop()
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	var buf []parkedBurl
	var timerC <-chan time.Time

	flush := func() {
		if len(buf) > 0 {
			b.flushFn(buf)
			buf = nil
		}
		if timerC != nil {
			if !timer.Stop() {
				<-timer.C
			}
			timerC = nil
		}
	}

	for {
		select {
		case p := <-b.parked:
			buf = append(buf, p)
			if len(buf) >= parkBatchMax {
				flush()
			} else if timerC == nil {
				timer.Reset(parkFlushInterval)
				timerC = timer.C
			}
		case <-timerC:
			timerC = nil
			if len(buf) > 0 {
				b.flushFn(buf)
				buf = nil
			}
		case <-sweep.C:
			b.sweepExpired()
		case <-b.stopped:
			// Drain anything already queued, then final flush.
			for {
				select {
				case p := <-b.parked:
					buf = append(buf, p)
				default:
					flush()
					return
				}
			}
		}
	}
}

// flushParked writes one multi-row INSERT for the batch. Same idempotence as
// the old per-row statement (PK conflict = NATS redelivery = no-op).
func (b *burlNotifier) flushParked(rows []parkedBurl) {
	traces := make([]string, len(rows))
	urls := make([]string, len(rows))
	for i, r := range rows {
		traces[i] = r.traceID
		urls[i] = r.billingURL
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := b.db.ExecContext(ctx, `
INSERT INTO burl_pending (trace_id, billing_url)
SELECT * FROM unnest($1::text[], $2::text[])
ON CONFLICT (trace_id) DO NOTHING`, pq.Array(traces), pq.Array(urls)); err != nil {
		b.log.Error("burl pending batch insert failed", "rows", len(rows), "error", err)
	}
}

// sweepExpired expires parks whose win never produced a billable impression.
func (b *burlNotifier) sweepExpired() {
	if b.db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := b.db.ExecContext(ctx,
		`DELETE FROM burl_pending WHERE created_at < now() - $1::interval`,
		burlPendingTTL.String()); err != nil {
		b.log.Warn("burl pending sweep failed", "error", err)
	}
}

// FireOnImpression claims and fires the trace's parked billing notice, if
// any. Called on EVERY impression — one DELETE ... RETURNING, almost always
// a zero-row miss (index probe, no WAL). The claim is what makes the fire
// exactly-once across replicas; the GET itself is async fire-and-forget.
// Never blocks or fails the impression (already recorded and billed).
func (b *burlNotifier) FireOnImpression(ctx context.Context, traceID string) {
	if b == nil || traceID == "" {
		return
	}
	var billingURL string
	err := b.db.QueryRowContext(ctx,
		`DELETE FROM burl_pending WHERE trace_id = $1 RETURNING billing_url`, traceID).Scan(&billingURL)
	if err == sql.ErrNoRows {
		return // no parked burl for this trace (or another replica claimed it)
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
