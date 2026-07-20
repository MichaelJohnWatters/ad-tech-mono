package main

// demo_trace.go — the staff-only guided "Auction Trace" demo.
//
// Sibling of demo_onboarding.go. Where that demo teaches how a first-party list
// becomes an audience, this one teaches how a SINGLE ad request flows through
// the platform: a persona browses, an ad request is fired at the SSP, the
// exchange runs an auction and fans out to DSPs, a winner is picked, the ad is
// served and the impression tracked, and finally the same trace_id flows on to
// hot rollups, the cold lake, and billing.
//
// It fires ONE real request through the REAL serve path (SSP /v1/ssp/serve)
// using a FIXED persona so the story is stable, captures the request's trace_id
// from the X-Trace-Id response header, then POLLS the REAL trace reader (the
// same reporting endpoint the trace explorer uses) until the impression event
// lands — narrating the async wait as its own step — and assembles the same
// 5-step {steps,summary} timeline the staff portal already renders.
//
// Staff-only: GET (support:read) returns the last-run snapshot; POST /run
// (support:update) fires + polls + assembles synchronously.

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
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/simulator/request"
)

// demoTracePersona is the fixed persona the demo fires. us-personalised-mobile
// is US-clear (full personalisation, no consent gating), UID2-addressable, and
// the highest-weighted persona in the registry — chosen because it reliably
// draws demand and WINS, so the auction story is stable across runs.
const demoTracePersona = "us-personalised-mobile"

// demoTracePlacement is the seeded display placement the simulator uses for the
// MPU/banner slot (profiles/publishers/standard.yaml). A broad, always-present
// slot so the demo can serve every time.
const demoTracePlacement = "pl-sim-mpu"

// demoTracePollTimeout bounds the async wait for the impression event to travel
// request → NATS → reporting. Kept short so the demo never hangs; if it lapses
// the demo narrates the wait honestly rather than failing.
const demoTracePollTimeout = 5 * time.Second

// demoTracePollInterval is how often we re-poll the trace reader while waiting.
const demoTracePollInterval = 250 * time.Millisecond

// demoTraceFireRetries is how many times the fire is retried when the persona
// no-bids before we give up and narrate the no-bid honestly. Fill can be flaky
// (budget depletion, freq caps), so a couple of retries makes the demo stable.
const demoTraceFireRetries = 3

// traceStepJSON mirrors reporting's traceStep (static/trace-render.js shape).
// Duplicated as a plain struct here so the orchestrator can parse the reporting
// trace response without importing the reporting package.
type traceStepJSON struct {
	Time    string `json:"time"`
	Service string `json:"service"`
	Cls     string `json:"cls"`
	Msg     string `json:"msg"`
	Detail  string `json:"detail"`
}

// tracePanelJSON mirrors reporting's tracePanel.
type tracePanelJSON struct {
	Title string `json:"title"`
	Value string `json:"value"`
	Label string `json:"label"`
	Cls   string `json:"cls"`
}

// traceSnapshot is the parsed reporting trace-reader response for one trace.
type traceSnapshot struct {
	TraceID string           `json:"traceId"`
	Note    string           `json:"note"`
	Steps   []traceStepJSON  `json:"steps"`
	Panels  []tracePanelJSON `json:"panels"`
}

// hasImpression reports whether the trace has reached the impression event yet
// (the async pipeline milestone the poll waits for).
func (t traceSnapshot) hasImpression() bool {
	for _, s := range t.Steps {
		if strings.Contains(strings.ToLower(s.Msg), "impression") {
			return true
		}
	}
	return false
}

// fireResult is what firing one request yields: the captured trace_id and
// whether the SSP actually served an ad (vs a no-bid).
type fireResult struct {
	TraceID string
	Served  bool
	// ClearingPriceCPM is the winning bid's CPM (price per 1000 impressions) from
	// the serve response — the human-legible auction figure, distinct from the
	// per-impression spend (CPM/1000) the trace panels carry.
	ClearingPriceCPM float64
	// FiredImpression is true when the demo fired the server-returned impression
	// beacon (what a browser render does), so the impression event will land.
	FiredImpression bool
}

// traceDemoBackend is the seam that keeps the demo testable: the LIVE path fires
// a real request through the SSP serve endpoint and reads the real trace reader;
// a fake drives the orchestrator in unit tests without a running stack.
type traceDemoBackend interface {
	// fireRequest fires ONE ad request for the fixed persona at the real serve
	// path and returns the captured trace_id + whether an ad served.
	fireRequest(ctx context.Context) (fireResult, error)
	// fetchTrace reads the (staff-unscoped) trace reader for traceID. A trace
	// with no events yet returns ok=false (events are still in flight).
	fetchTrace(ctx context.Context, traceID string) (snap traceSnapshot, ok bool, err error)
}

// traceDemoOrchestrator fires + polls + assembles. lastRun caches the most
// recent successful run so GET can replay it without re-firing (the run mutates
// real analytics state, so GET must not trigger it).
type traceDemoOrchestrator struct {
	backend traceDemoBackend
	log     *slog.Logger

	mu      sync.Mutex
	lastRun *demoResponse
}

// run fires one request, waits for the pipeline, and assembles the 5 steps.
func (o *traceDemoOrchestrator) run(ctx context.Context) (demoResponse, error) {
	// Fire, retrying a no-bid a few times before giving up — fill can be flaky.
	var fr fireResult
	var err error
	attempts := 0
	for attempts = 1; attempts <= demoTraceFireRetries; attempts++ {
		fr, err = o.backend.fireRequest(ctx)
		if err != nil {
			return demoResponse{}, fmt.Errorf("fire request: %w", err)
		}
		if fr.Served {
			break
		}
	}

	// Poll the trace reader until the impression event lands or we time out.
	// Even on a no-bid we still try to fetch: the auction/no-fill events carry
	// the story, and the trace may exist without an impression.
	snap, gotEvents := o.pollTrace(ctx, fr.TraceID)

	resp := assembleTraceSteps(fr, snap, gotEvents, attempts)
	now := time.Now().UTC()
	resp.RanAt = &now

	o.mu.Lock()
	cp := resp
	o.lastRun = &cp
	o.mu.Unlock()
	return resp, nil
}

// pollTrace polls the trace reader until the impression lands or the timeout
// lapses. Returns the last snapshot seen and whether any events arrived at all.
func (o *traceDemoOrchestrator) pollTrace(ctx context.Context, traceID string) (traceSnapshot, bool) {
	deadline := time.Now().Add(demoTracePollTimeout)
	var last traceSnapshot
	var any bool
	for {
		snap, ok, err := o.backend.fetchTrace(ctx, traceID)
		if err != nil {
			o.log.Warn("demo trace poll error", "trace_id", traceID, "error", err)
		}
		if ok {
			last = snap
			any = true
			if snap.hasImpression() {
				return last, true
			}
		}
		if time.Now().After(deadline) {
			return last, any
		}
		select {
		case <-ctx.Done():
			return last, any
		case <-time.After(demoTracePollInterval):
		}
	}
}

// currentState answers the GET: the cached last run, or a "not run yet" shape.
func (o *traceDemoOrchestrator) currentState() demoResponse {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.lastRun == nil {
		return demoResponse{Ran: false,
			Summary: "The demo has not been run yet. Click \"Run demo\" to fire one ad request through the live stack and watch its journey — auction, win, serve, impression — on a trace timeline."}
	}
	return *o.lastRun
}

// ---- step assembly (pure — the unit-tested core) ----

// tracePersonaSummary describes the fixed persona for the first step.
func tracePersonaSummary() map[string]any {
	p, _ := request.PersonaByName(demoTracePersona)
	return map[string]any{
		"name":      p.Name,
		"geo":       p.Geo,
		"region":    p.Region,
		"city":      p.City,
		"device":    p.Device,
		"os":        p.OS,
		"identity":  string(p.Identity),
		"regime":    string(p.Regime),
		"consent":   traceConsentLabel(p.Regime),
		"segments":  p.Segments,
		"placement": demoTracePlacement,
		"format":    string(request.Display),
	}
}

// traceConsentLabel turns a privacy regime into a one-line consent posture.
func traceConsentLabel(r request.Regime) string {
	switch r {
	case request.RegimeUSClear:
		return "US, no restriction — full personalisation"
	case request.RegimeGDPRConsented:
		return "GDPR, consent present — may personalise"
	case request.RegimeGDPRNoConsent:
		return "GDPR, no consent — contextual only"
	default:
		return string(r)
	}
}

// findStep returns the first trace step whose msg contains sub (case-insensitive)
// and whether one was found.
func findStep(steps []traceStepJSON, sub string) (traceStepJSON, bool) {
	sub = strings.ToLower(sub)
	for _, s := range steps {
		if strings.Contains(strings.ToLower(s.Msg), sub) {
			return s, true
		}
	}
	return traceStepJSON{}, false
}

// panelValue returns the value of the panel whose title contains sub, or "".
func panelValue(panels []tracePanelJSON, sub string) string {
	sub = strings.ToLower(sub)
	for _, p := range panels {
		if strings.Contains(strings.ToLower(p.Title), sub) {
			return p.Value
		}
	}
	return ""
}

// assembleTraceSteps builds the 5-step timeline from the fire result + the
// polled trace snapshot. Pure so it can be unit-tested against a synthetic
// snapshot (including the no-bid path). attempts is how many times the fire ran.
func assembleTraceSteps(fr fireResult, snap traceSnapshot, gotEvents bool, attempts int) demoResponse {
	steps := make([]demoStep, 0, 5)

	// --- Step 1: PERSONA — who's browsing + the placement/format. ---
	steps = append(steps, demoStep{
		N: 1, Title: "Persona — who's browsing",
		Narration: "A fixed, US-clear persona visits a publisher page. Same geo, device, identity and consent posture every run, so the story is stable. This is the user the ad request will describe.",
		Data:      tracePersonaSummary(),
	})

	// --- Step 2: REQUEST FIRED — the ad request hit the SSP; show trace_id. ---
	fireData := map[string]any{
		"trace_id": fr.TraceID,
		"endpoint": routes.SSPServe,
		"served":   fr.Served,
		"attempts": attempts,
		"explorer": routes.DevTraceExplorer + "?trace_id=" + fr.TraceID,
	}
	fireNarr := fmt.Sprintf("One ad request was fired at the SSP serve path (%s). Its trace_id — %s — tags every log line, NATS message and analytics row it touches from here on.", routes.SSPServe, fr.TraceID)
	if attempts > 1 {
		fireNarr += fmt.Sprintf(" (Fill was flaky — it took %d fires to win.)", attempts)
	}
	steps = append(steps, demoStep{N: 2, Title: "Request fired — the ad request hits the SSP", Narration: fireNarr, Data: fireData})

	// --- Step 3: AUCTION — SSP→Exchange→DSP fan-out→winner. ---
	winStep, hasWin := findStep(snap.Steps, "won")
	winner := panelValue(snap.Panels, "winner")
	spend := panelValue(snap.Panels, "spend")
	// The auction headline uses the CPM from the serve response (price per 1000
	// impressions — the figure buyers actually quote), not the per-impression
	// spend (CPM/1000) the trace panel carries, which rounds to $0.0000.
	cpm := "—"
	if fr.ClearingPriceCPM > 0 {
		cpm = fmt.Sprintf("$%.2f CPM", fr.ClearingPriceCPM)
	}
	auctionData := map[string]any{
		"winner_dsp":         winner,
		"clearing_price_cpm": cpm,
		"per_impression":     spend,
		"served":             fr.Served,
	}
	if hasWin {
		auctionData["time"] = winStep.Time
		auctionData["service"] = winStep.Service
		auctionData["detail"] = winStep.Detail
	}
	var auctionNarr string
	if hasWin {
		auctionNarr = fmt.Sprintf("The SSP handed the request to the exchange, which fanned out to the DSPs and ran a first-price auction (deal priority PG > Preferred > PMP > Open, then bid shading against the floor). %s won at %s — that's the price-per-1000; the per-impression cost is that ÷ 1000.", orDashTrace(winner), cpm)
	} else if fr.Served {
		auctionNarr = "The request served, but the auction-win event has not reached the analytics store yet — it is still travelling through NATS to reporting. Re-run to see the winner once it lands."
	} else {
		auctionNarr = "No DSP bid on this request (a genuine no-bid). Nothing served. This happens when demand is exhausted or a targeting/consent gate excluded every campaign — the demo persona is chosen to win reliably, so re-running usually fills."
	}
	steps = append(steps, demoStep{N: 3, Title: "Auction — exchange fan-out to a winner", Narration: auctionNarr, Data: auctionData})

	// --- Step 4: SERVE + TRACK — creative served, impression (+click/view). ---
	impStep, hasImp := findStep(snap.Steps, "impression")
	_, hasClick := findStep(snap.Steps, "click")
	viewStep, hasView := findStep(snap.Steps, "viewable")
	serveData := map[string]any{
		"served":     fr.Served,
		"impression": hasImp,
		"click":      hasClick,
		"viewable":   hasView,
	}
	if hasImp {
		serveData["impression_time"] = impStep.Time
		serveData["impression_detail"] = impStep.Detail
	}
	if hasView {
		serveData["view_verdict"] = viewStep.Msg
	}
	var serveNarr string
	switch {
	case hasImp:
		serveNarr = "The winning creative was served and the tracker recorded the impression — the billable event. Any click or viewability signal fired for this render shows here too."
	case fr.Served:
		serveNarr = "The ad served, but the impression event is still in flight to reporting. It will appear on a re-run once the pipeline catches up."
	default:
		serveNarr = "Nothing served, so there is no impression to track."
	}
	steps = append(steps, demoStep{N: 4, Title: "Serve + track — creative served, impression recorded", Narration: serveNarr, Data: serveData})

	// --- Step 5: WHERE THE DATA WENT — the lifecycle tie-back. ---
	steps = append(steps, demoStep{
		N: 5, Title: "Where the data went — one trace, many destinations",
		Narration: fmt.Sprintf("This same trace_id (%s) now flows on: into the hot rollups (dashboards read this in seconds), down to the cold lake (Parquet, for deep history and ML training), and into billing (the impression accrues advertiser spend and publisher revenue). One request, one id, tracked end to end — this is the lifecycle diagram, live.", fr.TraceID),
		Data: map[string]any{
			"trace_id":     fr.TraceID,
			"destinations": []string{"hot rollups (ClickHouse)", "cold lake (Parquet/Delta)", "billing ledger"},
		},
	})

	summary := fmt.Sprintf("persona → request → auction → serve → impression → pipeline. One real ad request (trace %s) went through the live stack: an auction picked a winner, the creative served, the tracker recorded the impression, and the same trace_id now feeds reporting, the lake, and billing.", fr.TraceID)
	if !fr.Served {
		summary = fmt.Sprintf("This run no-bid (trace %s): no DSP bid, so nothing served. Re-run — the demo persona is chosen to win reliably.", fr.TraceID)
	} else if !gotEvents {
		summary += " (Some events were still in flight when the poll timed out — re-run to see the full timeline.)"
	}

	return demoResponse{Ran: true, Steps: steps, Summary: summary}
}

func orDashTrace(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

// ---- HTTP handler ----

// demoTraceHandler serves both endpoints:
//
//	GET  /v1/api/demo/trace      (support:read)   — cached last-run snapshot
//	POST /v1/api/demo/trace/run  (support:update) — fire + poll + assemble
//
// Permission gating runs BEFORE the backend nil-check so a non-staff caller
// always gets 403 (never a 503 that would leak whether the backend is wired).
func demoTraceHandler(o *traceDemoOrchestrator) http.HandlerFunc {
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
				o.log.Error("demo trace run failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			o.log.Info("demo trace run", "trace_id", traceIDOf(resp), "served", servedOf(resp), "actor", claims.UserID)
			_ = json.NewEncoder(w).Encode(resp)
		}
	}
}

// traceIDOf / servedOf pull the run's trace_id + served flag out of step 2 for
// the audit log line (best-effort — never panics on an unexpected shape).
func traceIDOf(resp demoResponse) string {
	for _, s := range resp.Steps {
		if s.N == 2 {
			if m, ok := s.Data.(map[string]any); ok {
				if id, ok := m["trace_id"].(string); ok {
					return id
				}
			}
		}
	}
	return ""
}

func servedOf(resp demoResponse) bool {
	for _, s := range resp.Steps {
		if s.N == 2 {
			if m, ok := s.Data.(map[string]any); ok {
				if v, ok := m["served"].(bool); ok {
					return v
				}
			}
		}
	}
	return false
}

// ---- live backend (HTTP against the real SSP serve + reporting trace) ----

// httpTraceBackend is the LIVE backend: it fires a request at the SSP serve
// endpoint and reads the reporting trace reader — nothing mocked.
type httpTraceBackend struct {
	client       *http.Client
	sspURL       string
	reportingURL string
	log          *slog.Logger
}

// sspServeResult is the subset of the SSP serve JSON the demo needs. The
// impression/viewability URLs are the REAL HMAC-signed beacons the ad server
// built for this render — the demo fires them exactly like a browser would, so
// the impression event actually lands (firing /serve alone runs the auction but
// never records an impression).
type sspServeResult struct {
	NoBid          bool    `json:"nobid"`
	ImpressionURL  string  `json:"impression_url"`
	ViewabilityURL string  `json:"viewability_url"`
	ClearingPrice  float64 `json:"clearing_price"`
}

// fireRequest fires ONE display request at the SSP serve path for the fixed
// persona, capturing the trace_id from the X-Trace-Id response header.
func (b *httpTraceBackend) fireRequest(ctx context.Context) (fireResult, error) {
	p, ok := request.PersonaByName(demoTracePersona)
	if !ok {
		return fireResult{}, fmt.Errorf("unknown demo persona %q", demoTracePersona)
	}
	params := p.QueryParams(demoTracePlacement, request.Display)
	url := strings.TrimRight(b.sspURL, "/") + routes.SSPServe + "?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fireResult{}, err
	}
	// Browser-shaped so downstream fraud checks don't drop the request.
	req.Header.Set("User-Agent", "Mozilla/5.0 (adtech-demo)")
	req.Header.Set("Referer", "https://demo.adtech.local/")

	resp, err := b.client.Do(req)
	if err != nil {
		return fireResult{}, err
	}
	defer resp.Body.Close()
	traceID := resp.Header.Get(constants.HeaderTraceID)

	fr := fireResult{TraceID: traceID}
	if resp.StatusCode == http.StatusOK {
		var sr sspServeResult
		if err := json.NewDecoder(resp.Body).Decode(&sr); err == nil {
			fr.Served = !sr.NoBid
			fr.ClearingPriceCPM = sr.ClearingPrice
			if fr.Served {
				// Fire the server-returned beacons — this is what a browser does
				// on render; without it the auction runs but no impression is
				// ever recorded, so the trace's serve→impression half stays empty.
				if b.fireBeacon(ctx, sr.ImpressionURL) {
					fr.FiredImpression = true
				}
				b.fireBeacon(ctx, sr.ViewabilityURL) // best-effort; enriches the trace
			}
		}
	}
	if traceID == "" {
		return fireResult{}, fmt.Errorf("no %s header on serve response (status %d)", constants.HeaderTraceID, resp.StatusCode)
	}
	return fr, nil
}

// fireBeacon GETs a server-returned tracking URL (already HMAC-signed) the way a
// browser render would. Best-effort: a beacon failure just means the event
// won't show in the trace, which the poll narrates. Returns whether it fired OK.
func (b *httpTraceBackend) fireBeacon(ctx context.Context, beaconURL string) bool {
	beaconURL = strings.TrimSpace(beaconURL)
	if beaconURL == "" {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, beaconURL, nil)
	if err != nil {
		b.log.Warn("demo-trace: bad beacon url", "error", err)
		return false
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (adtech-demo)")
	resp, err := b.client.Do(req)
	if err != nil {
		b.log.Warn("demo-trace: beacon fire failed", "error", err)
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode < 400
}

// fetchTrace reads the reporting trace reader (the same endpoint the gateway
// proxies at /v1/api/trace) with STAFF-unscoped headers so the demo sees full
// detail. A 404 means the events have not landed yet → ok=false.
func (b *httpTraceBackend) fetchTrace(ctx context.Context, traceID string) (traceSnapshot, bool, error) {
	url := strings.TrimRight(b.reportingURL, "/") + routes.ReportingTrace + "?trace_id=" + traceID
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return traceSnapshot{}, false, err
	}
	// Staff/unscoped → the reporting trace handler returns full, unredacted
	// detail (winner DSP, clearing price, ids).
	req.Header.Set(constants.HeaderAccountType, string(auth.AccountStaff))

	resp, err := b.client.Do(req)
	if err != nil {
		return traceSnapshot{}, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return traceSnapshot{}, false, nil // events still in flight
	}
	if resp.StatusCode != http.StatusOK {
		return traceSnapshot{}, false, fmt.Errorf("trace fetch status %d", resp.StatusCode)
	}
	var snap traceSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		return traceSnapshot{}, false, err
	}
	return snap, true, nil
}
