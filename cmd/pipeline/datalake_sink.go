package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events/natsbus"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/datalake"
)

// startDatalakeSink wires the sink to NATS + object storage and starts the
// periodic flush. Fails open: if the object store or NATS is unavailable it
// logs and returns nil (the pipeline still serves health checks). Returns the
// sink so the caller can expose a snapshot/verification endpoint.
func startDatalakeSink(cfg *config.Config, log *slog.Logger, lc *lifecycle.Lifecycle) *datalakeSink {
	bucket := cfg.Get("pipeline.datalake_bucket", "adtech-datalake")
	objStore := connectObjects(cfg, log)
	if objStore == nil {
		log.Warn("datalake sink disabled: no object store")
		return nil
	}
	if err := objStore.EnsureBucket(context.Background(), bucket); err != nil {
		log.Warn("datalake: ensure bucket failed", "bucket", bucket, "error", err)
	}
	batchSize := cfg.GetInt("pipeline.datalake_batch_size", 500)
	sink := newDatalakeSink(datalake.NewObjectStore(objStore, bucket, log), batchSize, log)

	bus, err := natsbus.New(cfg.Get("pipeline.nats_url", routes.DefaultNATSURL), constants.ServicePipeline, log)
	if err != nil {
		log.Warn("datalake sink disabled: nats unavailable", "error", err)
		return nil
	}
	if err := bus.EnsureStream(context.Background(), events.StreamName, []string{events.StreamSubjects}); err != nil {
		log.Warn("datalake: ensure stream failed", "error", err)
	}
	if err := sink.Subscribe(bus); err != nil {
		log.Error("datalake sink subscribe failed", "error", err)
		_ = bus.Close()
		return nil
	}
	lc.OnShutdown("pipeline-nats", func(_ context.Context) error { return bus.Close() })

	flushInterval := cfg.GetDuration("pipeline.datalake_flush_interval", 30*time.Second)
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
	events.SubjectImpression: {"impressions", datalake.Schema{Version: 1, Columns: []datalake.Column{
		str("trace_id"), str("insertion_order_id"), str("campaign_id"), str("creative_id"),
		str("placement_id"), str("publisher_id"), str("account_id"), str("geo"), str("device"),
		str("channel"), str("format"), f64("clearing_price"), str("clearing_currency"),
		f64("clearing_price_usd"), str("bid_model"), str("deal_id"), ts("timestamp"),
	}}},
	events.SubjectClick: {"clicks", datalake.Schema{Version: 1, Columns: []datalake.Column{
		str("trace_id"), str("campaign_id"), str("creative_id"), str("placement_id"),
		str("publisher_id"), str("account_id"), str("landing_url"), str("geo"), str("device"), ts("timestamp"),
	}}},
	events.SubjectConversion: {"conversions", datalake.Schema{Version: 1, Columns: []datalake.Column{
		str("trace_id"), str("campaign_id"), str("creative_id"), str("placement_id"), str("account_id"),
		str("conversion_type"), f64("revenue"), str("currency"), f64("revenue_usd"), ts("timestamp"),
	}}},
	events.SubjectView: {"views", datalake.Schema{Version: 1, Columns: []datalake.Column{
		str("trace_id"), str("campaign_id"), str("creative_id"), str("placement_id"), str("publisher_id"),
		str("account_id"), i64("duration_ms"), i64("percent_visible"), i64("area_px"), boolC("iab_viewable"), ts("timestamp"),
	}}},
	events.SubjectAuctionWin: {"auction_wins", datalake.Schema{Version: 1, Columns: []datalake.Column{
		str("trace_id"), str("auction_id"), str("winner_dsp"), str("campaign_id"), str("creative_id"),
		str("placement_id"), str("publisher_id"), str("advertiser_id"), f64("clearing_price"),
		str("currency"), str("bid_model"), str("deal_id"), str("channel"), ts("timestamp"),
	}}},
	// The auction (ad-request) record — one per completed auction, filled or not.
	// Reporting lands the same event in its `auctions` table; mirroring it into
	// the lake lets the cold tier serve fill_rate (impressions/auctions) for deep
	// history, not just the hot window.
	events.SubjectAuctionComplete: {"auctions", datalake.Schema{Version: 1, Columns: []datalake.Column{
		str("trace_id"), str("placement_id"), str("publisher_id"), str("channel"),
		i64("num_bids"), f64("winning_bid"), f64("clearing_price"), str("currency"),
		f64("clearing_price_usd"), str("winner_dsp"), i64("duration_ms"), str("deal_id"), ts("timestamp"),
	}}},
	events.SubjectDSPCall: {"dsp_calls", datalake.Schema{Version: 1, Columns: []datalake.Column{
		str("trace_id"), str("auction_id"), str("channel"), str("dsp_endpoint"), boolC("bid_received"),
		f64("bid_price_usd"), i64("latency_ms"), boolC("timed_out"), ts("timestamp"),
	}}},
}

// datalakeSink buffers decoded events per table and flushes them to Parquet.
type datalakeSink struct {
	lake      datalake.Store
	batchSize int
	log       *slog.Logger

	mu      sync.Mutex
	buffers map[string][]datalake.Record
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
		buffers:   map[string][]datalake.Record{},
		schemas:   map[string]datalake.Schema{},
	}
	for _, t := range eventTables {
		s.schemas[t.table] = t.schema
	}
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
	return nil
}

func (s *datalakeSink) handlerFor(table string) events.Handler {
	return func(_ context.Context, msg *events.Message) error {
		var rec datalake.Record
		if err := json.Unmarshal(msg.Data, &rec); err != nil {
			s.log.Error("datalake sink: decode failed", "table", table, "error", err)
			return msg.Ack() // bad data — don't redeliver forever
		}
		s.record(table, rec)
		return msg.Ack()
	}
}

func (s *datalakeSink) record(table string, rec datalake.Record) {
	s.mu.Lock()
	s.buffers[table] = append(s.buffers[table], rec)
	full := len(s.buffers[table]) >= s.batchSize
	s.mu.Unlock()
	if full {
		s.flushTable(context.Background(), table)
	}
}

func (s *datalakeSink) flushTable(ctx context.Context, table string) {
	s.mu.Lock()
	recs := s.buffers[table]
	s.buffers[table] = nil
	s.mu.Unlock()
	if len(recs) == 0 {
		return
	}
	if err := s.lake.Write(ctx, table, recs, s.schemas[table]); err != nil {
		s.log.Error("datalake sink: write failed", "table", table, "records", len(recs), "error", err)
		return
	}
	s.log.Debug("datalake sink: flushed", "table", table, "records", len(recs))
}

// Flush writes every non-empty buffer. Called on the interval ticker and on
// graceful shutdown so buffered events aren't lost.
func (s *datalakeSink) Flush(ctx context.Context) {
	for table := range s.schemas {
		s.flushTable(ctx, table)
	}
}
