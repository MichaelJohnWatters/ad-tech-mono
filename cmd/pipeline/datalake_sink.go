package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events/natsbus"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/datalake"
)

// startDatalakeSink wires the sink to NATS + object storage and starts the
// periodic flush. Fails open: if the object store or NATS is unavailable it
// logs and returns nil (the pipeline still serves health checks). Returns the
// sink so the caller can expose a snapshot/verification endpoint.
func startDatalakeSink(cfg *config.Config, log *slog.Logger, lc *lifecycle.Lifecycle) *datalakeSink {
	bucket := keys.Pipeline.DatalakeBucket.Get(cfg)
	objStore := connectObjects(cfg, log)
	if objStore == nil {
		log.Warn("datalake sink disabled: no object store")
		return nil
	}
	if err := objStore.EnsureBucket(context.Background(), bucket); err != nil {
		log.Warn("datalake: ensure bucket failed", "bucket", bucket, "error", err)
	}
	batchSize := keys.Pipeline.DatalakeBatchSize.Get(cfg)
	sink := newDatalakeSink(datalake.NewObjectStore(objStore, bucket, log), batchSize, log)

	// One-time layout migration + NATS wiring run ASYNC: the migration
	// replays each table's delta log (an S3 GET per commit), which is
	// O(commits-since-last-compaction) — after an hour of 200rps load that
	// exceeded the liveness budget and crash-looped the pod at boot
	// (killed mid-migration at ~35s, forever). main() must reach the HTTP
	// server fast; the sink comes up when it comes up. Ordering INSIDE the
	// goroutine is preserved: migration strictly before the subscription,
	// so no flush interleaves with a rewrite (single-writer rule), and
	// JetStream holds the backlog until the subscription lands.
	go func() {
		for table, schema := range sink.schemas {
			if err := sink.lake.EnsurePartitioned(context.Background(), table, schema); err != nil {
				log.Error("datalake: partition migration failed", "table", table, "error", err)
			}
		}
		bus, err := natsbus.New(keys.Pipeline.NATSURL.Get(cfg), constants.ServicePipeline, log)
		if err != nil {
			log.Error("datalake sink disabled: nats unavailable", "error", err)
			return
		}
		if err := bus.EnsureStream(context.Background(), events.StreamName, []string{events.StreamSubjects}); err != nil {
			log.Warn("datalake: ensure stream failed", "error", err)
		}
		if err := sink.Subscribe(bus); err != nil {
			log.Error("datalake sink subscribe failed", "error", err)
			_ = bus.Close()
			return
		}
		lc.OnShutdown("pipeline-nats", func(_ context.Context) error { return bus.Close() })
		log.Info("datalake sink subscribed (post-migration)")
	}()

	// Keep this comfortably UNDER the NATS consumer AckWait (30s): with
	// ack-after-flush, an event stays un-acked until its flush, so a flush
	// interval >= AckWait lets JetStream redeliver before we ack (a harmless
	// duplicate, never a loss). 15s default gives margin.
	flushInterval := keys.Pipeline.DatalakeFlushInterval.Get(cfg)
	stop := make(chan struct{})
	lc.OnShutdown("datalake-sink", func(ctx context.Context) error {
		close(stop)
		sink.Flush(ctx) // final flush so buffered events aren't lost on shutdown
		return nil
	})
	go func() {
		t := time.NewTicker(flushInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				sink.Flush(context.Background())
			}
		}
	}()
	log.Info("datalake batch sink running", "bucket", bucket, "batch_size", batchSize, "flush_interval", flushInterval.String())
	return sink
}

// Snapshot flushes buffered records for the table then returns the Parquet
// table snapshot (active file set + total rows/bytes from the Delta log). The
// flush makes the count immediately consistent — the verification affordance
// the /debug/datalake/snapshot endpoint and e2e "did every event land?" checks
// rely on, rather than waiting for the interval ticker.
func (s *datalakeSink) Snapshot(ctx context.Context, table string) (*datalake.TableSnapshot, error) {
	s.flushTable(ctx, table)
	return s.lake.Snapshot(ctx, table)
}

// Tables returns the datalake table names the sink writes.
func (s *datalakeSink) Tables() []string {
	out := make([]string, 0, len(s.schemas))
	for t := range s.schemas {
		out = append(out, t)
	}
	return out
}

// The pipeline is the data lake's *batch layer*: it consumes the same event
// stream reporting does (its own durable NATS group, so both see every
// event) and lands the events as columnar Parquet in object storage via the
// Delta-logged datalake store. Reporting is the speed layer (real-time
// queries); this is the durable, replayable, ML/backfill-friendly copy.
//
// Events are buffered per table and flushed to Parquet on a size or time
// trigger — one Parquet file per flush keeps files a sensible size rather
// than one-row-per-file.

func str(name string) datalake.Column {
	return datalake.Column{Name: name, Type: "string", Nullable: true}
}
func f64(name string) datalake.Column {
	return datalake.Column{Name: name, Type: "float64", Nullable: true}
}
func i64(name string) datalake.Column {
	return datalake.Column{Name: name, Type: "int64", Nullable: true}
}
func boolC(name string) datalake.Column {
	return datalake.Column{Name: name, Type: "bool", Nullable: true}
}
func ts(name string) datalake.Column {
	return datalake.Column{Name: name, Type: "timestamp", Nullable: true}
}

// eventTables maps a NATS subject → (datalake table, schema). Column names
// match the analytics event JSON tags (reporting decodes the same wire
// shapes), so a raw json.Unmarshal into a Record already has the right keys.
var eventTables = map[string]struct {
	table  string
	schema datalake.Schema
}{
	events.SubjectImpression: {"impressions", datalake.Schema{Version: 1, PartitionBy: "timestamp", Columns: []datalake.Column{
		str("trace_id"), str("insertion_order_id"), str("campaign_id"), str("creative_id"),
		str("placement_id"), str("publisher_id"), str("account_id"), str("geo"), str("device"),
		str("channel"), str("format"), f64("clearing_price"), str("clearing_currency"),
		f64("clearing_price_usd"), str("bid_model"), str("deal_id"), ts("timestamp"),
	}}},
	events.SubjectClick: {"clicks", datalake.Schema{Version: 1, PartitionBy: "timestamp", Columns: []datalake.Column{
		str("trace_id"), str("campaign_id"), str("creative_id"), str("placement_id"),
		str("publisher_id"), str("account_id"), str("landing_url"), str("geo"), str("device"), ts("timestamp"),
	}}},
	events.SubjectConversion: {"conversions", datalake.Schema{Version: 1, PartitionBy: "timestamp", Columns: []datalake.Column{
		str("trace_id"), str("campaign_id"), str("creative_id"), str("placement_id"), str("account_id"),
		str("conversion_type"), f64("revenue"), str("currency"), f64("revenue_usd"), ts("timestamp"),
	}}},
	events.SubjectView: {"views", datalake.Schema{Version: 1, PartitionBy: "timestamp", Columns: []datalake.Column{
		str("trace_id"), str("campaign_id"), str("creative_id"), str("placement_id"), str("publisher_id"),
		str("account_id"), i64("duration_ms"), i64("percent_visible"), i64("area_px"), boolC("iab_viewable"), ts("timestamp"),
	}}},
	events.SubjectAuctionWin: {"auction_wins", datalake.Schema{Version: 1, PartitionBy: "timestamp", Columns: []datalake.Column{
		str("trace_id"), str("auction_id"), str("winner_dsp"), str("campaign_id"), str("creative_id"),
		str("placement_id"), str("publisher_id"), str("advertiser_id"), f64("clearing_price"),
		str("currency"), str("bid_model"), str("deal_id"), str("channel"), ts("timestamp"),
	}}},
	// The auction (ad-request) record — one per completed auction, filled or not.
	// Reporting lands the same event in its `auctions` table; mirroring it into
	// the lake lets the cold tier serve fill_rate (impressions/auctions) for deep
	// history, not just the hot window.
	events.SubjectAuctionComplete: {"auctions", datalake.Schema{Version: 1, PartitionBy: "timestamp", Columns: []datalake.Column{
		str("trace_id"), str("placement_id"), str("publisher_id"), str("channel"),
		i64("num_bids"), f64("winning_bid"), f64("clearing_price"), str("currency"),
		f64("clearing_price_usd"), str("winner_dsp"), i64("duration_ms"), str("deal_id"), ts("timestamp"),
	}}},
	events.SubjectDSPCall: {"dsp_calls", datalake.Schema{Version: 1, PartitionBy: "timestamp", Columns: []datalake.Column{
		str("trace_id"), str("auction_id"), str("channel"), str("dsp_endpoint"), boolC("bid_received"),
		f64("bid_price_usd"), i64("latency_ms"), boolC("timed_out"), ts("timestamp"),
	}}},
	// Consent-gated behavioural signals (SSP request rows + tracker
	// interaction rows) — the input to behavioural segmentation rules. The
	// ONLY lake table besides profile_signals that carries user keys, so both
	// are covered by the GDPR purge (privacy.go).
	events.SubjectBehaviourObserved: {behaviourSignalsTable, datalake.Schema{Version: 1, PartitionBy: "observed_at", Columns: []datalake.Column{
		str("trace_id"), str("kind"), str("user_id"), str("household_id"),
		str("placement_id"), str("publisher_id"), str("campaign_id"), str("creative_id"),
		str("channel"), str("categories"), str("geo"), str("device"),
		str("account_id"), str("tag"), ts("observed_at"),
	}}},
}

const behaviourSignalsTable = "behaviour_signals"

// profileSignalsTable is the normalized onboarding-signal table — the
// append-only record of "id X was declared a member of segment S by account A
// via source Z". Unlike the event tables above, its NATS messages are BATCHES
// (one ProfileSignalEvent per upload chunk), expanded here into one row per
// id, so it gets a dedicated handler rather than the generic 1-msg-1-row one.
// The drop-zone poller (onboarding.go) writes the same rows directly via
// record() — same table, same schema, no NATS hop.
const profileSignalsTable = "profile_signals"

var profileSignalsSchema = datalake.Schema{Version: 1, PartitionBy: "observed_at", Columns: []datalake.Column{
	str("trace_id"), str("id_type"), str("id_value"), str("source"), str("access"),
	str("account_id"), str("provider"), str("segment_id"), str("segment_name"),
	str("visibility"), boolC("consent"), ts("observed_at"),
}}

// profileSignalRecord builds one profile_signals lake row. Shared by the NATS
// batch expander and the drop-zone poller so both write the same shape.
func profileSignalRecord(ev events.ProfileSignalEvent, id events.ProfileSignalID) datalake.Record {
	return datalake.Record{
		"trace_id": ev.TraceID, "id_type": id.IDType, "id_value": id.IDValue,
		"source": ev.Source, "access": ev.Access, "account_id": ev.AccountID,
		"provider": ev.Provider, "segment_id": ev.SegmentID, "segment_name": ev.SegmentName,
		"visibility": ev.Visibility, "consent": ev.Consent, "observed_at": ev.ObservedAt,
	}
}

// bufferedEvent is a decoded record awaiting flush, paired with its NATS
// ack/nak. The ack is deferred until the record is DURABLY written (ack-after-
// flush): on a successful flush we ack, on a failed flush we nak so JetStream
// redelivers. This makes the lake at-least-once — a crash/restart with a
// non-empty buffer redelivers the un-acked events instead of losing them.
// (ack/nak are nil for the direct record() path used by tests.)
type bufferedEvent struct {
	rec datalake.Record
	ack func() error
	nak func() error
}

// datalakeSink buffers decoded events per table and flushes them to Parquet.
type datalakeSink struct {
	lake      datalake.Store
	batchSize int
	log       *slog.Logger

	mu      sync.Mutex
	buffers map[string][]bufferedEvent
	schemas map[string]datalake.Schema
}

func newDatalakeSink(lake datalake.Store, batchSize int, log *slog.Logger) *datalakeSink {
	if batchSize < 1 {
		batchSize = 1
	}
	s := &datalakeSink{
		lake:      lake,
		batchSize: batchSize,
		log:       log,
		buffers:   map[string][]bufferedEvent{},
		schemas:   map[string]datalake.Schema{},
	}
	for _, t := range eventTables {
		s.schemas[t.table] = t.schema
	}
	s.schemas[profileSignalsTable] = profileSignalsSchema
	return s
}

// Subscribe wires a handler per event subject onto the bus. The pipeline
// uses its own consumer group (ServicePipeline) so JetStream delivers every
// event to both reporting and the pipeline independently.
func (s *datalakeSink) Subscribe(bus events.EventBus) error {
	for subject, t := range eventTables {
		if err := bus.Subscribe(context.Background(), subject, constants.ServicePipeline, s.handlerFor(t.table)); err != nil {
			return err
		}
		s.log.Info("datalake sink subscribed", "subject", subject, "table", t.table)
	}
	if err := bus.Subscribe(context.Background(), events.SubjectProfileSignal, constants.ServicePipeline, s.profileSignalHandler()); err != nil {
		return err
	}
	s.log.Info("datalake sink subscribed", "subject", events.SubjectProfileSignal, "table", profileSignalsTable)
	return nil
}

// profileSignalHandler expands a ProfileSignalEvent batch into one lake row
// per id. The message's ack/nak rides on the LAST row of the batch: the ack
// only fires once that row's flush durably lands, and a failed flush naks the
// whole message for redelivery. A batch split across two flushes where the
// first succeeds and the second fails therefore redelivers the entire batch —
// duplicate appended rows, never lost ones (memberships are idempotent and
// the profile-builder reconcile pass dedupes on read).
func (s *datalakeSink) profileSignalHandler() events.Handler {
	return func(_ context.Context, msg *events.Message) error {
		var ev events.ProfileSignalEvent
		if err := json.Unmarshal(msg.Data, &ev); err != nil {
			s.log.Error("datalake sink: profile signal decode failed", "error", err)
			return msg.Ack() // bad data — don't redeliver forever
		}
		if len(ev.IDs) == 0 {
			return msg.Ack()
		}
		for i, id := range ev.IDs {
			be := bufferedEvent{rec: profileSignalRecord(ev, id)}
			if i == len(ev.IDs)-1 {
				be.ack, be.nak = msg.Ack, msg.Nak
			}
			s.bufferEvent(profileSignalsTable, be)
		}
		return nil
	}
}

func (s *datalakeSink) handlerFor(table string) events.Handler {
	return func(_ context.Context, msg *events.Message) error {
		var rec datalake.Record
		if err := json.Unmarshal(msg.Data, &rec); err != nil {
			s.log.Error("datalake sink: decode failed", "table", table, "error", err)
			return msg.Ack() // bad data — don't redeliver forever
		}
		// Buffer WITH the message's ack/nak; do NOT ack here. The ack fires only
		// once flushTable durably writes this record (ack-after-flush) — so a
		// crash/restart with a non-empty buffer redelivers instead of losing.
		s.bufferEvent(table, bufferedEvent{rec: rec, ack: msg.Ack, nak: msg.Nak})
		return nil
	}
}

// dropBuffered discards a table's buffered-but-unflushed rows (harness
// reset: the rows' source events are being wiped wholesale; acking them
// away is fine because the reset destroys their destination too).
func (s *datalakeSink) dropBuffered(table string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ev := range s.buffers[table] {
		if ev.ack != nil {
			_ = ev.ack()
		}
	}
	delete(s.buffers, table)
}

// record buffers a bare record with no ack/nak — the direct path used by tests.
func (s *datalakeSink) record(table string, rec datalake.Record) {
	s.bufferEvent(table, bufferedEvent{rec: rec})
}

func (s *datalakeSink) bufferEvent(table string, ev bufferedEvent) {
	s.mu.Lock()
	s.buffers[table] = append(s.buffers[table], ev)
	full := len(s.buffers[table]) >= s.batchSize
	s.mu.Unlock()
	if full {
		s.flushTable(context.Background(), table)
	}
}

func (s *datalakeSink) flushTable(ctx context.Context, table string) {
	s.mu.Lock()
	evs := s.buffers[table]
	s.buffers[table] = nil
	s.mu.Unlock()
	if len(evs) == 0 {
		return
	}
	recs := make([]datalake.Record, len(evs))
	for i, e := range evs {
		recs[i] = e.rec
	}
	if err := s.lake.Write(ctx, table, recs, s.schemas[table]); err != nil {
		// Don't lose the events: nak so JetStream redelivers them (at-least-once).
		s.log.Error("datalake sink: write failed, naking for redelivery", "table", table, "records", len(evs), "error", err)
		for _, e := range evs {
			if e.nak != nil {
				_ = e.nak()
			}
		}
		return
	}
	// Durably written — now it's safe to ack.
	for _, e := range evs {
		if e.ack != nil {
			_ = e.ack()
		}
	}
	s.log.Debug("datalake sink: flushed", "table", table, "records", len(evs))
}

// Flush writes every non-empty buffer. Called on the interval ticker and on
// graceful shutdown so buffered events aren't lost.
func (s *datalakeSink) Flush(ctx context.Context) {
	for table := range s.schemas {
		s.flushTable(ctx, table)
	}
}
