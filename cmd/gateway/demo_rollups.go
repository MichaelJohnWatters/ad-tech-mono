package main

// demo_rollups.go — the staff-only guided "Rollups" demo.
//
// Sibling of demo_onboarding.go / demo_trace.go / demo_billing.go. Where those
// follow one request (or one dollar) through the platform, this one teaches how
// RAW events collapse into far fewer AGGREGATED rows — same totals, fewer rows —
// as you climb the rollup ladder, and how reporting auto-picks a tier by the
// query's time range.
//
// It proves the collapse against the REAL stack without touching the rollup
// ENGINE at all. On the ClickHouse backend the engine's InsertRollups path is a
// no-op (rollups are maintained by MATERIALIZED VIEWS on insert), so triggering
// it shows nothing. The honest, reliable collapse is instead read straight off
// the reporting QUERY API:
//
//   - RAW rows:  {table:"impressions", metrics:["count"]}         → one row,
//     rows[0][0] = the total raw impression row count.
//   - AGGREGATED rows: the same query GROUPed BY the rollup dimensions
//     (campaign_id, creative_id, placement_id, geo, device) → ONE row per
//     (campaign,creative,placement,geo,device) tuple. So len(rows) is exactly
//     the aggregated row count an hourly rollup would store, and the SUM of the
//     count column across those rows equals the raw total — same totals, fewer
//     rows. A coarser group-by (campaign_id, geo only) yields even fewer rows:
//     climbing the ladder trades granularity for storage.
//
// The flow:
//  1. THE FIREHOSE   — fire N real fixed-persona impressions (real ~sub-cent
//                      spend on whoever wins). Raw events are one row per event.
//  2. RAW LANDED     — the raw impression row count from the query API (and,
//                      best-effort, that it rose by ~N).
//  3. THE COLLAPSE   — grouped query: N_raw raw rows → N_agg aggregated rows, a
//                      N_raw/N_agg× reduction, and SUM(counts)==N_raw. THIS is
//                      what one rollup row is.
//  4. COARSER = FEWER — a SECOND grouped query with fewer dimensions → even
//                      fewer rows.
//  5. THE LADDER     — the retention table + how reporting's AutoTier picks the
//                      coarsest tier that answers a range and HotColdStore routes
//                      recent→ClickHouse / aged→lake.
//
// Honesty: the collapse queries cover ALL traffic in the window, not just the
// demo's N — the demo says so. If a query fails or the window is empty, the
// step narrates that honestly with a re-run hint; no numbers are fabricated.
//
// Staff-only: GET (support:read) returns the last-run snapshot; POST /run
// (support:update) fires + queries + assembles synchronously.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
)

// demoRollupsFireN is how many real impressions the firehose fires. Kept small —
// each is real ~sub-cent spend on whoever wins — while still visibly nudging the
// raw row count.
const demoRollupsFireN = 8

// demoRollupsLandWait is how long we wait after firing for the impressions to
// travel serve → tracker → NATS → reporting → the analytics store before the
// raw-count query runs. Best-effort: if they haven't all landed we narrate the
// wait honestly rather than fabricate the delta. A var so unit tests can shrink
// it (the fake backend needs no real settle time).
var demoRollupsLandWait = 2 * time.Second

// demoRollupsWindow scopes the collapse queries to a window with data (the last
// 24h). Only time_from is set (not time_to) so the AutoTier builder falls back
// to a raw store.Query — giving exact per-group counts, so len(rows) is the true
// aggregated row count rather than a rollup-tier re-aggregation.
const demoRollupsWindow = 24 * time.Hour

// The rollup dimensions the fine collapse groups by — exactly the entity
// dimensions the hourly impressions rollup stores (impressions_rollup_hourly:
// campaign_id, creative_id, placement_id, geo, device). One row per tuple IS one
// rollup row. Note the impressions schema column is `device`, not `device_type`.
var demoRollupFineDims = []string{"campaign_id", "creative_id", "placement_id", "geo", "device"}

// demoRollupCoarseDims is the coarser tier: drop creative_id + placement_id, so
// grouping by campaign_id + geo yields even fewer rows.
var demoRollupCoarseDims = []string{"campaign_id", "geo"}

// fireOutcome is the result of the firehose: how many impressions were fired vs
// actually served (won an auction).
type fireOutcome struct {
	Fired  int
	Served int
}

// countResult is one query-API answer: whether it succeeded, the raw total
// (SUM of the count column) and the number of returned rows. For an ungrouped
// count query Rows==1 and Total==rows[0][0]; for a grouped query Rows is the
// aggregated row count and Total is SUM(count) across the groups.
type countResult struct {
	OK    bool
	Total int64
	Rows  int
}

// rollupsDemoBackend is the seam that keeps the demo testable: the LIVE path
// fires real impressions through the SSP serve path and reads the real reporting
// query API; a fake drives the orchestrator in unit tests without a running
// stack.
type rollupsDemoBackend interface {
	// fireImpressions fires n real fixed-persona impressions (serve + impression
	// beacon) and reports how many fired and how many served (won).
	fireImpressions(ctx context.Context, n int) fireOutcome
	// rawCount asks the query API for the total raw impression row count in the
	// window (ungrouped count).
	rawCount(ctx context.Context, since time.Time) countResult
	// groupedCount asks the query API for the count grouped by dims — Rows is the
	// aggregated row count, Total is SUM(count) across the groups.
	groupedCount(ctx context.Context, since time.Time, dims []string) countResult
}

// rollupsDemoOrchestrator fires + queries + assembles. lastRun caches the most
// recent run so GET replays it without re-firing (a run fires real impressions,
// so GET must not trigger it).
type rollupsDemoOrchestrator struct {
	backend rollupsDemoBackend
	log     *slog.Logger

	mu      sync.Mutex
	lastRun *demoResponse
}

// run fires the firehose, waits for it to land, runs the raw + two grouped
// count queries, and assembles the 5 steps.
func (o *rollupsDemoOrchestrator) run(ctx context.Context) (demoResponse, error) {
	// Step 1 — the firehose. Real impressions, real spend.
	fire := o.backend.fireImpressions(ctx, demoRollupsFireN)

	// Give the events time to travel the pipeline before we count them.
	select {
	case <-ctx.Done():
		return demoResponse{}, ctx.Err()
	case <-time.After(demoRollupsLandWait):
	}

	since := time.Now().Add(-demoRollupsWindow)
	raw := o.backend.rawCount(ctx, since)
	fine := o.backend.groupedCount(ctx, since, demoRollupFineDims)
	coarse := o.backend.groupedCount(ctx, since, demoRollupCoarseDims)

	resp := assembleRollupSteps(fire, raw, fine, coarse)
	o.cache(&resp)
	return resp, nil
}

// cache stores the run so a subsequent GET replays it without re-firing.
func (o *rollupsDemoOrchestrator) cache(resp *demoResponse) {
	now := time.Now().UTC()
	resp.RanAt = &now
	o.mu.Lock()
	cp := *resp
	o.lastRun = &cp
	o.mu.Unlock()
}

// currentState answers the GET: the cached last run, or a "not run yet" shape.
func (o *rollupsDemoOrchestrator) currentState() demoResponse {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.lastRun == nil {
		return demoResponse{Ran: false,
			Summary: fmt.Sprintf("The demo has not been run yet. Click \"Run demo\" to fire %d real impressions and watch the reporting query API collapse the raw event rows into far fewer aggregated rows — same totals, fewer rows — one row per rollup dimension-tuple.", demoRollupsFireN)}
	}
	return *o.lastRun
}

// ---- step assembly (pure — the unit-tested core) ----

// reductionRatio returns raw/agg as a float, or 0 when agg is 0 (avoids a
// divide-by-zero and signals "no collapse to show").
func reductionRatio(raw, agg int64) float64 {
	if agg <= 0 {
		return 0
	}
	return float64(raw) / float64(agg)
}

// assembleRollupSteps builds the 5-step timeline from the fire outcome + the
// three count-query results. Pure so it can be unit-tested against synthetic
// counts (including the query-failed and no-served paths).
func assembleRollupSteps(fire fireOutcome, raw, fine, coarse countResult) demoResponse {
	steps := make([]demoStep, 0, 5)

	// --- Step 1: THE FIREHOSE — N real impressions, one raw row each. ---
	step1 := demoStep{
		N: 1, Title: "The firehose — every event is a row",
		Data: map[string]any{
			"persona":   demoTracePersona,
			"placement": demoTracePlacement,
			"fired":     fire.Fired,
			"served":    fire.Served,
			"tier":      "raw impressions",
		},
	}
	if fire.Served > 0 {
		step1.Narration = fmt.Sprintf("We fired %d real ad requests from a fixed persona; %d won an auction and rendered, writing one raw impression row each. This is REAL traffic — a real ~sub-cent drawdown on whoever won. The raw tier is one row per event: the most detailed, queryable to the individual impression, and the most expensive to store.", fire.Fired, fire.Served)
	} else {
		step1.Narration = fmt.Sprintf("We fired %d real ad requests from a fixed persona but none won an auction (a genuine no-bid — demand exhausted or a targeting gate). Nothing new landed in the raw tier. The collapse below still reads whatever traffic is already in the window; re-run to add fresh impressions.", fire.Fired)
	}
	steps = append(steps, step1)

	// --- Step 2: RAW LANDED — the raw impression row count from the query API. ---
	step2 := demoStep{
		N: 2, Title: "Raw landed — one row per impression",
		Data: map[string]any{
			"raw_rows":  raw.Total,
			"query_ok":  raw.OK,
			"window_h":  int(demoRollupsWindow.Hours()),
			"tier":      "raw impressions",
			"queryable": "individual event",
		},
	}
	if raw.OK {
		step2.Narration = fmt.Sprintf("The reporting query API — {table:\"impressions\", metrics:[\"count\"]} over the last %dh — reports %s raw impression rows across ALL traffic in the window (not just this demo's %d). Every one is queryable down to the individual event. That granularity is the whole point of the raw tier — and why it's the tier you retain for the shortest time.", int(demoRollupsWindow.Hours()), fmtN(raw.Total), fire.Served)
	} else {
		step2.Narration = "The raw-count query against the reporting query API failed or returned nothing — reporting may still be catching up, or the window has no traffic. No number is shown rather than a fabricated one. Re-run once impressions have landed."
	}
	steps = append(steps, step2)

	// --- Step 3: THE COLLAPSE — raw rows → aggregated rows, same totals. ---
	ratio := reductionRatio(raw.Total, int64(fine.Rows))
	totalsMatch := fine.OK && raw.OK && fine.Total == raw.Total
	step3 := demoStep{
		N: 3, Title: "The collapse — same totals, far fewer rows",
		Data: map[string]any{
			"raw_rows":         raw.Total,
			"aggregated_rows":  fine.Rows,
			"reduction":        ratio,
			"grouped_total":    fine.Total,
			"totals_match":     totalsMatch,
			"dimensions":       demoRollupFineDims,
			"query_ok":         fine.OK,
			"tier":             "hourly rollup",
			"rollup_row_means": "one (campaign, creative, placement, geo, device) tuple per time bucket",
		},
	}
	switch {
	case fine.OK && fine.Rows > 0:
		matchNote := "and the SUM of those grouped counts EQUALS the raw total — same totals, fewer rows."
		if !totalsMatch {
			matchNote = fmt.Sprintf("(grouped total %s vs raw %s — a small gap means the window shifted between the two queries; re-run to line them up).", fmtN(fine.Total), fmtN(raw.Total))
		}
		step3.Narration = fmt.Sprintf("Now group the SAME impressions by the rollup dimensions (%s). The query returns ONE row per (campaign, creative, placement, geo, device) tuple: %s raw rows collapse to %s aggregated rows — a %.0f× reduction — %s THIS aggregated row IS what an hourly rollup stores: one row per dimension-tuple per time bucket.", strings.Join(demoRollupFineDims, ", "), fmtN(raw.Total), fmtN(int64(fine.Rows)), ratio, matchNote)
	case fine.OK:
		step3.Narration = "The grouped query succeeded but returned no rows — the window has no impressions to collapse. Re-run once traffic has landed."
	default:
		step3.Narration = "The grouped collapse query against the reporting query API failed — no reduction number is shown rather than a fabricated one. Re-run once reporting is reachable and impressions have landed."
	}
	steps = append(steps, step3)

	// --- Step 4: COARSER = FEWER — drop dimensions, get even fewer rows. ---
	step4 := demoStep{
		N: 4, Title: "Coarser = fewer — climbing the ladder",
		Data: map[string]any{
			"fine_dimensions":   demoRollupFineDims,
			"fine_rows":         fine.Rows,
			"coarse_dimensions": demoRollupCoarseDims,
			"coarse_rows":       coarse.Rows,
			"coarse_total":      coarse.Total,
			"query_ok":          coarse.OK,
		},
	}
	switch {
	case coarse.OK && coarse.Rows > 0:
		step4.Narration = fmt.Sprintf("Drop creative_id and placement_id — group by just %s — and the same traffic collapses further: %s rows → %s rows. Each rung of the ladder drops dimensions, trading granularity for storage. The totals still match; you just can't ask \"which creative?\" of the coarser tier.", strings.Join(demoRollupCoarseDims, " + "), fmtN(int64(fine.Rows)), fmtN(int64(coarse.Rows)))
	case coarse.OK:
		step4.Narration = "The coarser grouped query returned no rows — no traffic in the window to collapse further. Re-run once impressions have landed."
	default:
		step4.Narration = "The coarser grouped query failed — no number is shown rather than a fabricated one. Re-run once reporting is reachable."
	}
	steps = append(steps, step4)

	// --- Step 5: THE LADDER — retention tiers + AutoTier + hot/cold routing. ---
	steps = append(steps, demoStep{
		N: 5, Title: "The ladder — a tier for every question",
		Narration: "Each tier is retained for a different horizon: raw for 24–48h (the firehose, then dropped), minute for 7 days, hourly for 90 days, daily for 2 years, monthly forever. When you query, reporting's AutoTier picks the COARSEST tier that can still answer your range — a last-hour query hits raw/minute; a two-year query hits daily. And HotColdStore routes recent windows to ClickHouse (hot) and aged windows to the Parquet lake (cold), transparently. This is the rollup stage of the data-lifecycle diagram, live: fewer rows as you climb, identical totals, the right tier auto-selected per question.",
		Data: map[string]any{
			"retention": []map[string]string{
				{"tier": "raw", "retention": "24–48h", "row_shape": "one per event"},
				{"tier": "minute", "retention": "7 days", "row_shape": "per dim-tuple per minute"},
				{"tier": "hourly", "retention": "90 days", "row_shape": "per dim-tuple per hour"},
				{"tier": "daily", "retention": "2 years", "row_shape": "per dim-tuple per day"},
				{"tier": "monthly", "retention": "forever", "row_shape": "per dim-tuple per month"},
			},
			"auto_tier": "reporting picks the coarsest tier that answers the query's range",
			"hot_cold":  "HotColdStore routes recent→ClickHouse, aged→Parquet lake",
		},
	})

	// Summary.
	var summary string
	switch {
	case fine.OK && fine.Rows > 0 && raw.OK:
		summary = fmt.Sprintf("firehose → raw rows → collapse → coarser → ladder. %s raw impression rows collapsed to %s aggregated rows (a %.0f× reduction) grouped by campaign/creative/placement/geo/device, with the totals matching — one aggregated row IS one rollup row. Drop two dimensions and it collapses to %s rows. Each rung is retained longer; reporting auto-picks the coarsest tier that answers a query.", fmtN(raw.Total), fmtN(int64(fine.Rows)), ratio, fmtN(int64(coarse.Rows)))
	case !raw.OK || !fine.OK:
		summary = "The reporting query API didn't return a clean answer this run (still catching up, or an empty window). No numbers were fabricated — re-run once impressions have landed to see the raw→aggregated collapse with matching totals."
	default:
		summary = "The window held no impressions to collapse. Re-run — the firehose fires real impressions, then the query API shows raw rows collapsing into far fewer aggregated rows with identical totals."
	}

	return demoResponse{Ran: true, Steps: steps, Summary: summary}
}

// fmtN renders a count with thousands separators for the narration.
func fmtN(n int64) string {
	s := fmt.Sprintf("%d", n)
	if n < 0 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}

// ---- HTTP handler ----

// demoRollupsHandler serves both endpoints:
//
//	GET  /v1/api/demo/rollups      (support:read)   — cached last-run snapshot
//	POST /v1/api/demo/rollups/run  (support:update) — fire + query + assemble
//
// Permission gating runs BEFORE the backend nil-check so a non-staff caller
// always gets 403 (never a 503 that would leak whether the backend is wired).
func demoRollupsHandler(o *rollupsDemoOrchestrator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")

		switch r.Method {
		case http.MethodGet:
			if !can(claims, "support:read") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
		case http.MethodPost:
			if !can(claims, "support:update") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}

		if o == nil || o.backend == nil {
			http.Error(w, `{"error":"demo unavailable"}`, http.StatusServiceUnavailable)
			return
		}

		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(o.currentState())
		case http.MethodPost:
			resp, err := o.run(r.Context())
			if err != nil {
				o.log.Error("demo rollups run failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			o.log.Info("demo rollups run", "steps", len(resp.Steps), "actor", claims.UserID)
			_ = json.NewEncoder(w).Encode(resp)
		}
	}
}

// ---- live backend (HTTP serve/beacon + the real reporting query API) ----

// httpRollupsBackend is the LIVE backend: it fires real impressions via the
// shared serveOnce/fireBeacon and reads the real reporting query API. Nothing
// mocked.
type httpRollupsBackend struct {
	client       *http.Client
	sspURL       string
	reportingURL string
	log          *slog.Logger
}

// fireImpressions fires n real fixed-persona requests at the serve path and, for
// each that served, fires the server-returned impression beacon the way a
// browser render would — so a raw impression row lands per served request.
func (b *httpRollupsBackend) fireImpressions(ctx context.Context, n int) fireOutcome {
	var out fireOutcome
	for i := 0; i < n; i++ {
		if ctx.Err() != nil {
			break
		}
		out.Fired++
		sr, err := serveOnce(ctx, b.client, b.sspURL, demoTracePersona, demoTracePlacement)
		if err != nil {
			b.log.Warn("demo rollups: serve failed", "error", err)
			continue
		}
		if !sr.Served {
			continue
		}
		if fireBeacon(ctx, b.client, b.log, sr.ImpressionURL) {
			out.Served++
		}
	}
	return out
}

// rawCount asks the query API for the total raw impression row count in the
// window: {table:"impressions", metrics:["count"], time_from:since}. One row,
// rows[0][0] = the total.
func (b *httpRollupsBackend) rawCount(ctx context.Context, since time.Time) countResult {
	res, err := b.query(ctx, analytics.QueryParams{
		Table: "impressions", Metrics: []string{"count"}, TimeFrom: since,
	})
	if err != nil {
		b.log.Warn("demo rollups: raw count query failed", "error", err)
		return countResult{}
	}
	total, _ := sumCountColumn(res)
	return countResult{OK: true, Total: total, Rows: len(res.Rows)}
}

// groupedCount asks the query API for the count grouped by dims. Rows is the
// aggregated row count (one per dimension-tuple); Total is SUM(count) across the
// groups — which equals the raw total, proving "same totals, fewer rows".
func (b *httpRollupsBackend) groupedCount(ctx context.Context, since time.Time, dims []string) countResult {
	res, err := b.query(ctx, analytics.QueryParams{
		Table: "impressions", Metrics: []string{"count"}, Dimensions: dims, TimeFrom: since,
	})
	if err != nil {
		b.log.Warn("demo rollups: grouped count query failed", "dims", dims, "error", err)
		return countResult{}
	}
	total, _ := sumCountColumn(res)
	return countResult{OK: true, Total: total, Rows: len(res.Rows)}
}

// query POSTs a QueryParams to the reporting query API with STAFF-unscoped
// headers (so the demo sees platform-wide totals, matching what a rollup row
// aggregates) and decodes the {columns,rows} result.
func (b *httpRollupsBackend) query(ctx context.Context, params analytics.QueryParams) (analytics.QueryResult, error) {
	body, err := json.Marshal(params)
	if err != nil {
		return analytics.QueryResult{}, err
	}
	url := strings.TrimRight(b.reportingURL, "/") + routes.ReportingQuery
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return analytics.QueryResult{}, err
	}
	req.Header.Set(constants.HeaderContentType, constants.ContentTypeJSON)
	req.Header.Set(constants.HeaderAccountType, string(auth.AccountStaff))

	resp, err := b.client.Do(req)
	if err != nil {
		return analytics.QueryResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return analytics.QueryResult{}, fmt.Errorf("query status %d", resp.StatusCode)
	}
	var out analytics.QueryResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return analytics.QueryResult{}, err
	}
	return out, nil
}

// sumCountColumn sums the "count" column across all rows of a query result. For
// an ungrouped count the sole row's count is the total; for a grouped count the
// sum across group rows is the raw total. Returns (0,false) if there's no count
// column.
func sumCountColumn(res analytics.QueryResult) (int64, bool) {
	idx := -1
	for i, c := range res.Columns {
		if c == "count" {
			idx = i
			break
		}
	}
	if idx < 0 {
		return 0, false
	}
	var total int64
	for _, row := range res.Rows {
		if idx < len(row) {
			total += toInt64(row[idx])
		}
	}
	return total, true
}

// toInt64 coerces a JSON-decoded numeric cell (float64 by default, but ints or
// numeric strings can appear across backends) to int64. Non-numeric → 0.
func toInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	case string:
		var f float64
		if _, err := fmt.Sscanf(n, "%g", &f); err == nil {
			return int64(f)
		}
	}
	return 0
}
