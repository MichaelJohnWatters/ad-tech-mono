package analytics

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ColdReader answers historical queries from cold storage (the Parquet data
// lake). It's implemented by pkg/store/datalake (behind the `duckdb` build tag);
// when cold isn't available it's nil and the HotColdStore degrades to hot-only.
type ColdReader interface {
	Query(ctx context.Context, params QueryParams) (*QueryResult, error)
}

// HotColdStore routes reads by time: rows within hotWindow of now come from the
// hot store (ClickHouse), older rows from cold (the Parquet lake). WRITES ALWAYS
// go to hot — the pipeline independently writes the lake, so the hot/cold store
// never writes cold. A query spanning the hot/cold boundary is split, run
// against both stores, and merged additively by dimension key.
//
// It implements analytics.Store, so it drops in wherever a Store is expected
// (the reporting query path, the rollup Builder's raw fallback). The tenant
// scope in params.Filters rides through both sub-queries unchanged.
type HotColdStore struct {
	hot       Store
	cold      ColdReader
	hotWindow time.Duration
	now       func() time.Time
	log       *slog.Logger
}

// NewHotColdStore wraps a hot store with a cold reader. If cold is nil the store
// is hot-only (every read passes straight through). hotWindow is how far back
// the hot store is authoritative (e.g. 7d). log may be nil.
func NewHotColdStore(hot Store, cold ColdReader, hotWindow time.Duration, log *slog.Logger) *HotColdStore {
	if log == nil {
		log = slog.Default()
	}
	return &HotColdStore{hot: hot, cold: cold, hotWindow: hotWindow, now: time.Now, log: log}
}

// Query routes by time range. Boundary = now − hotWindow; rows at or after it
// are hot, strictly before it are cold.
func (t *HotColdStore) Query(ctx context.Context, params QueryParams) (*QueryResult, error) {
	if t.cold == nil {
		return t.hot.Query(ctx, params)
	}
	// Hot-only tables never reach the cold lake (see exportTables) — routing
	// their queries by time would serve a silent EMPTY answer for any range the
	// router deems cold, even though the rows exist in hot ClickHouse right up
	// to its TTL. Serve them entirely hot, original params, no Approximate
	// stamp: hot holds everything that survives for these tables.
	if hotOnlyTables[params.Table] {
		return t.hot.Query(ctx, params)
	}
	boundary := t.now().Add(-t.hotWindow)
	to := params.TimeTo
	if to.IsZero() {
		to = t.now()
	}

	// Entirely in one store → single pass, native result (no float coercion).
	if !params.TimeFrom.IsZero() && !params.TimeFrom.Before(boundary) {
		return t.hot.Query(ctx, params) // from >= boundary
	}
	if !to.After(boundary) {
		return t.cold.Query(ctx, params) // to <= boundary
	}

	// Spanning the boundary. Non-additive metrics (avg_*) can't be merged across
	// stores correctly, so serve them from hot only (documented approximation —
	// the derived-metric engine only ever asks for additive base metrics).
	if hasNonAdditive(params.Metrics) {
		t.log.Warn("hotcold: non-additive metric over a hot/cold span — serving hot only (approximate)",
			"table", params.Table, "metrics", params.Metrics)
		res, err := t.hot.Query(ctx, params)
		if res != nil {
			res.Approximate = "non-additive metrics over a hot/cold span: hot window only"
		}
		return res, err
	}

	// Split: hot owns [boundary, to], cold owns [from, boundary). Cold's upper
	// bound is exclusive (boundary − 1ns) so a row exactly at the boundary is
	// counted once (hot), never double-counted. Limit/order are dropped on the
	// sub-queries and re-applied to the merged result.
	hotParams := params
	hotParams.TimeFrom = boundary
	hotParams.Limit, hotParams.OrderBy, hotParams.OrderDir = 0, "", ""

	coldParams := params
	coldParams.TimeTo = boundary.Add(-time.Nanosecond)
	coldParams.Limit, coldParams.OrderBy, coldParams.OrderDir = 0, "", ""

	hotRes, err := t.hot.Query(ctx, hotParams)
	if err != nil {
		return nil, err
	}
	degraded := ""
	coldRes, err := t.cold.Query(ctx, coldParams)
	if err != nil {
		// Cold is best-effort (S3/DuckDB may be down): degrade to the hot half
		// rather than fail the whole query — but SAY SO on the wire, not just
		// in a log nobody reads at query time.
		t.log.Warn("hotcold: cold query failed, serving hot window only", "table", params.Table, "error", err)
		coldRes = &QueryResult{}
		degraded = "cold store unavailable: hot window only (history missing from this answer)"
	}

	merged := mergeAdditive(params.Dimensions, params.Metrics, hotRes, coldRes)
	applyHotColdOrderLimit(merged, params.OrderBy, params.OrderDir, params.Limit)
	merged.Approximate = degraded
	return merged, nil
}

// HotStore exposes the wrapped hot store so callers can reach capabilities the
// wrapper doesn't forward method-by-method (e.g. the DebugReader read-backs —
// debug reads are recent-window by definition, so the hot store is always the
// right answerer). Without this, wrapping ClickHouse in HotColdStore hid its
// DebugReader and the /debug endpoints 501'd on the full-local stack.
func (t *HotColdStore) HotStore() Store { return t.hot }

// Warm-start aggregators — optional capabilities discovered by type
// assertion on the reporting store, so the wrapper must forward them or
// wrapping ClickHouse silently strips them: the exchange routing
// warm-start/reseed and the ad-server bandit warm-start 501'd whenever
// reporting.cold_store_enabled was on. Both windows are minutes-to-hours,
// squarely inside the hot window, so hot-only is the right answerer.
var (
	_ DSPCallAggregator      = (*HotColdStore)(nil)
	_ CreativeStatAggregator = (*HotColdStore)(nil)
)

func (t *HotColdStore) DSPCallStats(ctx context.Context, since time.Time) ([]DSPCallStat, error) {
	agg, ok := t.hot.(DSPCallAggregator)
	if !ok {
		return nil, fmt.Errorf("hot store does not aggregate dsp_calls")
	}
	return agg.DSPCallStats(ctx, since)
}

func (t *HotColdStore) CreativeStats(ctx context.Context, since time.Time) ([]CreativeStat, error) {
	agg, ok := t.hot.(CreativeStatAggregator)
	if !ok {
		return nil, fmt.Errorf("hot store does not aggregate creative stats")
	}
	return agg.CreativeStats(ctx, since)
}

// ObservabilityWriter — operational signals (no-fills, freq-cap blocks,
// render failures, rejections, state changes, depletions) forward to the hot
// store. Without this the consumer's store.(ObservabilityWriter) assertion
// failed on the wrapped store and every operational event was silently
// dropped on the full-local stack. Fire-and-forget like the interface: a hot
// store without the capability just doesn't record them.
func (t *HotColdStore) InsertServeNoFill(n ServeNoFill) {
	if ow, ok := t.hot.(ObservabilityWriter); ok {
		ow.InsertServeNoFill(n)
	}
}
func (t *HotColdStore) InsertFreqCapBlock(b FreqCapBlock) {
	if ow, ok := t.hot.(ObservabilityWriter); ok {
		ow.InsertFreqCapBlock(b)
	}
}
func (t *HotColdStore) InsertRenderFailure(r RenderFailure) {
	if ow, ok := t.hot.(ObservabilityWriter); ok {
		ow.InsertRenderFailure(r)
	}
}
func (t *HotColdStore) InsertTrackerRejection(r TrackerRejection) {
	if ow, ok := t.hot.(ObservabilityWriter); ok {
		ow.InsertTrackerRejection(r)
	}
}
func (t *HotColdStore) InsertCampaignStateChange(cs CampaignStateChange) {
	if ow, ok := t.hot.(ObservabilityWriter); ok {
		ow.InsertCampaignStateChange(cs)
	}
}
func (t *HotColdStore) InsertBudgetDepletion(b BudgetDepletion) {
	if ow, ok := t.hot.(ObservabilityWriter); ok {
		ow.InsertBudgetDepletion(b)
	}
}

// CommittedByCampaign delegates the shared-pacing-counter reconcile query to the
// hot store (ClickHouse holds today's raw impressions). Committed spend is always
// "today", which is inside the hot window, so cold is never involved. Returns nil
// (additive-only, no store self-heal) when the hot store doesn't implement
// CommittedReader — the wrapper must forward the capability or reporting can't see
// it through HotColdStore.
func (t *HotColdStore) CommittedByCampaign(ctx context.Context, day string) (map[string]int64, error) {
	cr, ok := t.hot.(CommittedReader)
	if !ok {
		return nil, nil
	}
	return cr.CommittedByCampaign(ctx, day)
}

// ImpressionsByPublisher delegates the tiered-revenue-share month count to the
// hot store (this month's impressions live in the hot window). Returns nil when
// the hot store doesn't implement PublisherImpressionReader.
func (t *HotColdStore) ImpressionsByPublisher(ctx context.Context, since time.Time) (map[string]int64, error) {
	pr, ok := t.hot.(PublisherImpressionReader)
	if !ok {
		return nil, nil
	}
	return pr.ImpressionsByPublisher(ctx, since)
}

// ViewableImpressionsForUsers delegates view-through lookback to the hot store —
// attribution windows (≤30d) sit well inside the hot window. Returns nil when the
// hot store doesn't implement ViewThroughReader.
func (t *HotColdStore) ViewableImpressionsForUsers(ctx context.Context, userIDs []string, accountID, campaignID string, since time.Time, requireViewable bool) ([]ViewableImpression, error) {
	vr, ok := t.hot.(ViewThroughReader)
	if !ok {
		return nil, nil
	}
	return vr.ViewableImpressionsForUsers(ctx, userIDs, accountID, campaignID, since, requireViewable)
}

// InsertAttributionTouchpoints writes the multi-touch chain to the hot store.
func (t *HotColdStore) InsertAttributionTouchpoints(ctx context.Context, rows []*AttributionTouchpointRow) error {
	aw, ok := t.hot.(AttributionWriter)
	if !ok {
		return nil
	}
	return aw.InsertAttributionTouchpoints(ctx, rows)
}

// AttributionChain reads the conversion's multi-touch chain from the hot store.
func (t *HotColdStore) AttributionChain(ctx context.Context, conversionTraceID string) ([]AttributionTouchpointRow, error) {
	ar, ok := t.hot.(AttributionReader)
	if !ok {
		return nil, nil
	}
	return ar.AttributionChain(ctx, conversionTraceID)
}

// EventsByTrace / RecentImpressions (TraceReader) delegate to the hot store — a
// trace inspector only ever looks at recent (hot-window) data, so cold is never
// involved. Forwarding here keeps the capability visible through the wrapper.
func (t *HotColdStore) EventsByTrace(ctx context.Context, traceID string, scope TraceScope) ([]TraceEvent, error) {
	tr, ok := t.hot.(TraceReader)
	if !ok {
		return nil, nil
	}
	return tr.EventsByTrace(ctx, traceID, scope)
}

func (t *HotColdStore) RecentImpressions(ctx context.Context, scope TraceScope, limit int) ([]ImpressionRow, error) {
	tr, ok := t.hot.(TraceReader)
	if !ok {
		return nil, nil
	}
	return tr.RecentImpressions(ctx, scope, limit)
}

// Write path + lifecycle all delegate to hot (the lake is written by the pipeline).
func (t *HotColdStore) InsertImpression(ctx context.Context, e *ImpressionEvent) error {
	return t.hot.InsertImpression(ctx, e)
}
func (t *HotColdStore) InsertClick(ctx context.Context, e *ClickEvent) error {
	return t.hot.InsertClick(ctx, e)
}
func (t *HotColdStore) InsertConversion(ctx context.Context, e *ConversionEvent) error {
	return t.hot.InsertConversion(ctx, e)
}
func (t *HotColdStore) InsertView(ctx context.Context, e *ViewEvent) error {
	return t.hot.InsertView(ctx, e)
}
func (t *HotColdStore) InsertAuction(ctx context.Context, e *AuctionEvent) error {
	return t.hot.InsertAuction(ctx, e)
}
func (t *HotColdStore) InsertAuctionWin(ctx context.Context, e *AuctionWinEvent) error {
	return t.hot.InsertAuctionWin(ctx, e)
}
func (t *HotColdStore) InsertMediaEvent(ctx context.Context, e *MediaEvent) error {
	return t.hot.InsertMediaEvent(ctx, e)
}
func (t *HotColdStore) InsertBatch(ctx context.Context, events []Event) error {
	return t.hot.InsertBatch(ctx, events)
}

// Compile-time guard: HotColdStore MUST satisfy BatchInserter, else reporting's
// bulk consumer silently falls back to the slow per-message path at runtime.
var _ BatchInserter = (*HotColdStore)(nil)

// BatchInserter — reporting's high-volume bulk consumer type-asserts the store
// to BatchInserter. When the hot store is wrapped here those methods must exist,
// or the assertion fails and reporting silently drops to the slow per-message
// path (~2 events/sec, and it piles up ClickHouse MergeTree parts). Delegate to
// the hot store's bulk inserts (ClickHouse supports them); per-row fallback
// otherwise.
func (t *HotColdStore) InsertImpressions(ctx context.Context, es []*ImpressionEvent) error {
	if bi, ok := t.hot.(BatchInserter); ok {
		return bi.InsertImpressions(ctx, es)
	}
	for _, e := range es {
		if err := t.hot.InsertImpression(ctx, e); err != nil {
			return err
		}
	}
	return nil
}
func (t *HotColdStore) InsertClicks(ctx context.Context, es []*ClickEvent) error {
	if bi, ok := t.hot.(BatchInserter); ok {
		return bi.InsertClicks(ctx, es)
	}
	for _, e := range es {
		if err := t.hot.InsertClick(ctx, e); err != nil {
			return err
		}
	}
	return nil
}
func (t *HotColdStore) InsertConversions(ctx context.Context, es []*ConversionEvent) error {
	if bi, ok := t.hot.(BatchInserter); ok {
		return bi.InsertConversions(ctx, es)
	}
	for _, e := range es {
		if err := t.hot.InsertConversion(ctx, e); err != nil {
			return err
		}
	}
	return nil
}
func (t *HotColdStore) InsertViews(ctx context.Context, es []*ViewEvent) error {
	if bi, ok := t.hot.(BatchInserter); ok {
		return bi.InsertViews(ctx, es)
	}
	for _, e := range es {
		if err := t.hot.InsertView(ctx, e); err != nil {
			return err
		}
	}
	return nil
}
func (t *HotColdStore) InsertAuctions(ctx context.Context, es []*AuctionEvent) error {
	if bi, ok := t.hot.(BatchInserter); ok {
		return bi.InsertAuctions(ctx, es)
	}
	for _, e := range es {
		if err := t.hot.InsertAuction(ctx, e); err != nil {
			return err
		}
	}
	return nil
}
func (t *HotColdStore) InsertAuctionWins(ctx context.Context, es []*AuctionWinEvent) error {
	if bi, ok := t.hot.(BatchInserter); ok {
		return bi.InsertAuctionWins(ctx, es)
	}
	for _, e := range es {
		if err := t.hot.InsertAuctionWin(ctx, e); err != nil {
			return err
		}
	}
	return nil
}
func (t *HotColdStore) InsertMediaEvents(ctx context.Context, es []*MediaEvent) error {
	if bi, ok := t.hot.(BatchInserter); ok {
		return bi.InsertMediaEvents(ctx, es)
	}
	for _, e := range es {
		if err := t.hot.InsertMediaEvent(ctx, e); err != nil {
			return err
		}
	}
	return nil
}
func (t *HotColdStore) InsertDSPCalls(ctx context.Context, es []*DSPCallEvent) error {
	if bi, ok := t.hot.(BatchInserter); ok {
		return bi.InsertDSPCalls(ctx, es)
	}
	return nil // Store has no single-row DSPCall insert; hot always supports batch in practice
}

// Profile-store tables (ADR 0006 phase 1): delegate to the hot store's bulk
// insert (ClickHouse). Like DSPCalls, the base Store has no single-row insert
// for these, and the hot backend always supports BatchInserter in practice.
func (t *HotColdStore) InsertBehaviourSignals(ctx context.Context, es []*BehaviourSignalRow) error {
	if bi, ok := t.hot.(BatchInserter); ok {
		return bi.InsertBehaviourSignals(ctx, es)
	}
	return nil
}
func (t *HotColdStore) InsertProfileSignals(ctx context.Context, es []*ProfileSignalRow) error {
	if bi, ok := t.hot.(BatchInserter); ok {
		return bi.InsertProfileSignals(ctx, es)
	}
	return nil
}

// Close closes the hot store; if the cold reader owns a closable handle it's
// closed too (idempotent — the caller may also close it).
func (t *HotColdStore) Close() error {
	if c, ok := t.cold.(interface{ Close() error }); ok {
		_ = c.Close()
	}
	return t.hot.Close()
}

// additiveMetrics are the base metrics that sum across stores/rollups. avg_* are
// deliberately absent — a mean can't be re-derived by adding two means.
// sum_viewable and the media countIfs are plain sums/counts and merge fine.
var additiveMetrics = map[string]bool{
	"count": true, "sum_cost": true, "sum_revenue": true, "sum_bids": true,
	"sum_viewable": true, "media_starts": true, "media_completes": true,
}

// hotOnlyTables are the tables absent from exportTables — observability spines
// that live only in hot ClickHouse (bounded by its TTL) and have no parquet
// cold path. Derived from the export list at init so the two can't drift.
var hotOnlyTables = func() map[string]bool {
	exported := map[string]bool{}
	for _, t := range exportTables {
		exported[t.name] = true
	}
	out := map[string]bool{}
	for _, name := range []string{
		"media_events", "serve_no_fills", "freq_cap_blocks", "render_failures",
		"campaign_state_changes", "budget_depletions", "tracker_rejections",
	} {
		if !exported[name] {
			out[name] = true
		}
	}
	return out
}()

func hasNonAdditive(metrics []string) bool {
	for _, m := range metrics {
		if !additiveMetrics[m] {
			return true
		}
	}
	return false
}

// mergeAdditive folds one or more results into one, summing metric columns by
// dimension-key. Column order is canonical: dimensions then metrics. Values come
// out as float64 (the only type that survives summing int + float rows from two
// backends); the reporting engine and JSON both accept that.
func mergeAdditive(dims, metrics []string, parts ...*QueryResult) *QueryResult {
	type acc struct {
		dimVals []interface{}
		mets    map[string]float64
	}
	accs := map[string]*acc{}
	var order []string

	for _, part := range parts {
		if part == nil {
			continue
		}
		colIdx := map[string]int{}
		for i, c := range part.Columns {
			colIdx[c] = i
		}
		for _, row := range part.Rows {
			dimVals := make([]interface{}, len(dims))
			keyParts := make([]string, len(dims))
			for i, d := range dims {
				if ci, ok := colIdx[d]; ok && ci < len(row) {
					dimVals[i] = row[ci]
					keyParts[i] = toStringKey(row[ci])
				}
			}
			key := strings.Join(keyParts, "\x00")
			a := accs[key]
			if a == nil {
				a = &acc{dimVals: dimVals, mets: map[string]float64{}}
				accs[key] = a
				order = append(order, key)
			}
			for _, m := range metrics {
				if ci, ok := colIdx[m]; ok && ci < len(row) {
					a.mets[m] += toFloatVal(row[ci])
				}
			}
		}
	}

	columns := append(append([]string{}, dims...), metrics...)
	res := &QueryResult{Columns: columns}
	for _, key := range order {
		a := accs[key]
		row := append([]interface{}{}, a.dimVals...)
		for _, m := range metrics {
			row = append(row, a.mets[m])
		}
		res.Rows = append(res.Rows, row)
	}
	return res
}

// applyHotColdOrderLimit sorts the merged rows by a metric column and truncates —
// order/limit can't be pushed down when two stores are merged, so it's applied
// once on the combined result.
func applyHotColdOrderLimit(res *QueryResult, orderBy, orderDir string, limit int) {
	if res == nil {
		return
	}
	if orderBy != "" {
		ci := -1
		for i, c := range res.Columns {
			if c == orderBy {
				ci = i
				break
			}
		}
		if ci >= 0 {
			sort.SliceStable(res.Rows, func(i, j int) bool {
				a, b := toFloatVal(res.Rows[i][ci]), toFloatVal(res.Rows[j][ci])
				if orderDir == "desc" {
					return a > b
				}
				return a < b
			})
		}
	}
	if limit > 0 && len(res.Rows) > limit {
		res.Rows = res.Rows[:limit]
	}
}

func toStringKey(v interface{}) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return strconv.FormatFloat(toFloatVal(v), 'g', -1, 64)
}

func toFloatVal(v interface{}) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case int32:
		return float64(n)
	case uint64:
		return float64(n)
	case uint32:
		return float64(n)
	case uint:
		return float64(n)
	case string:
		f, _ := strconv.ParseFloat(n, 64)
		return f
	default:
		return 0
	}
}
