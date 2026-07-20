package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// fakeTraceBackend drives the orchestrator in unit tests without a live stack.
type fakeTraceBackend struct {
	fire  fireResult
	fireN int // increments each fire, so we can assert retries
	snap  traceSnapshot
	ok    bool
}

func (f *fakeTraceBackend) fireRequest(context.Context) (fireResult, error) {
	f.fireN++
	return f.fire, nil
}

func (f *fakeTraceBackend) fetchTrace(context.Context, string) (traceSnapshot, bool, error) {
	return f.snap, f.ok, nil
}

// TestDemoTraceHandler_Permissions mirrors the onboarding demo's gating: staff
// read may VIEW, staff update may RUN; everyone else is 403; the backend
// nil-check sits behind the permission gate (403 never leaks wiring).
func TestDemoTraceHandler_Permissions(t *testing.T) {
	o := &traceDemoOrchestrator{backend: nil, log: quietLog()} // nil backend → 503 after gate
	h := demoTraceHandler(o)

	req := func(method, target string, claims *auth.Claims) *http.Request {
		r := httptest.NewRequest(method, target, nil)
		if claims != nil {
			r = withClaims(r, claims)
		}
		return r
	}

	staffRead := &auth.Claims{AccountType: auth.AccountStaff, Permissions: []string{"support:read"}}
	staffUpdate := &auth.Claims{AccountType: auth.AccountStaff, Permissions: []string{"support:read", "support:update"}}
	advertiser := &auth.Claims{AccountType: auth.AccountAdvertiser, Permissions: []string{"reports:read"}}

	cases := []struct {
		name   string
		method string
		target string
		claims *auth.Claims
		want   int
	}{
		{"no claims → 401", http.MethodGet, routes.APIDemoTrace, nil, http.StatusUnauthorized},
		{"advertiser view → 403", http.MethodGet, routes.APIDemoTrace, advertiser, http.StatusForbidden},
		{"advertiser run → 403", http.MethodPost, routes.APIDemoTraceRun, advertiser, http.StatusForbidden},
		// support:read may VIEW but NOT run (support:update required).
		{"read-only staff run → 403", http.MethodPost, routes.APIDemoTraceRun, staffRead, http.StatusForbidden},
		// With the right permission, gating passes and we reach the nil-backend 503.
		{"staff run reaches backend → 503", http.MethodPost, routes.APIDemoTraceRun, staffUpdate, http.StatusServiceUnavailable},
		{"unsupported method → 405", http.MethodDelete, routes.APIDemoTrace, staffUpdate, http.StatusMethodNotAllowed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h(rec, req(tc.method, tc.target, tc.claims))
			if rec.Code != tc.want {
				t.Fatalf("code = %d, want %d (body=%s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// TestDemoTraceHandler_ViewBeforeRun asserts a staff GET before any run returns
// a valid "not run yet" shape (200, ran=false) rather than 503 — the backend is
// wired, there's just no cached run.
func TestDemoTraceHandler_ViewBeforeRun(t *testing.T) {
	o := &traceDemoOrchestrator{backend: &fakeTraceBackend{}, log: quietLog()}
	h := demoTraceHandler(o)
	r := withClaims(httptest.NewRequest(http.MethodGet, routes.APIDemoTrace, nil),
		&auth.Claims{AccountType: auth.AccountStaff, Permissions: []string{"support:read"}})
	rec := httptest.NewRecorder()
	h(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
}

// syntheticWinTrace is a full happy-path trace snapshot (staff-unredacted shape)
// with an auction win, an impression, a viewable verdict and a click.
func syntheticWinTrace(id string) traceSnapshot {
	return traceSnapshot{
		TraceID: id,
		Steps: []traceStepJSON{
			{Time: "0ms", Service: "exchange", Msg: "Auction won by dsp-alpha", Detail: "clearing $2.50 CPM · advertiser abcdef12… · campaign 99887766…"},
			{Time: "+12ms", Service: "tracker", Msg: "Impression recorded", Detail: "creative cr-123 · cpm"},
			{Time: "+40ms", Service: "tracker", Msg: "Viewable impression (IAB)"},
			{Time: "+80ms", Service: "tracker", Msg: "Click"},
		},
		Panels: []tracePanelJSON{
			{Title: "Advertiser spend", Value: "$0.0025"},
			{Title: "Winner", Value: "dsp-alpha"},
			{Title: "Publisher rev", Value: "$0.0018"},
		},
	}
}

// TestAssembleTraceSteps_Win exercises the happy path: five steps, real values
// pulled from the trace (winner, price, impression, view, click), served=true.
func TestAssembleTraceSteps_Win(t *testing.T) {
	fr := fireResult{TraceID: "trace-abc", Served: true, ClearingPriceCPM: 2.5, FiredImpression: true}
	resp := assembleTraceSteps(fr, syntheticWinTrace("trace-abc"), true, 1)

	if !resp.Ran {
		t.Fatal("Ran = false, want true")
	}
	if len(resp.Steps) != 5 {
		t.Fatalf("steps = %d, want 5", len(resp.Steps))
	}
	// Step numbers 1..5 in order.
	for i, s := range resp.Steps {
		if s.N != i+1 {
			t.Fatalf("step[%d].N = %d, want %d", i, s.N, i+1)
		}
	}

	// Step 2 carries the trace_id + served + the SSP endpoint.
	d2 := resp.Steps[1].Data.(map[string]any)
	if d2["trace_id"] != "trace-abc" {
		t.Errorf("step2 trace_id = %v, want trace-abc", d2["trace_id"])
	}
	if d2["endpoint"] != routes.SSPServe {
		t.Errorf("step2 endpoint = %v, want %s", d2["endpoint"], routes.SSPServe)
	}
	if d2["served"] != true {
		t.Errorf("step2 served = %v, want true", d2["served"])
	}

	// Step 3 carries the real winner + clearing price from the trace panels.
	d3 := resp.Steps[2].Data.(map[string]any)
	if d3["winner_dsp"] != "dsp-alpha" {
		t.Errorf("step3 winner_dsp = %v, want dsp-alpha", d3["winner_dsp"])
	}
	if d3["clearing_price_cpm"] != "$2.50 CPM" {
		t.Errorf("step3 clearing_price_cpm = %v, want $2.50 CPM", d3["clearing_price_cpm"])
	}
	if d3["per_impression"] != "$0.0025" {
		t.Errorf("step3 per_impression = %v, want $0.0025", d3["per_impression"])
	}

	// Step 4 reflects impression + view + click flags from the trace steps.
	d4 := resp.Steps[3].Data.(map[string]any)
	if d4["impression"] != true || d4["viewable"] != true || d4["click"] != true {
		t.Errorf("step4 flags = %+v, want impression/viewable/click all true", d4)
	}

	// Step 5 lists the three destinations.
	d5 := resp.Steps[4].Data.(map[string]any)
	if dests, ok := d5["destinations"].([]string); !ok || len(dests) != 3 {
		t.Errorf("step5 destinations = %v, want 3", d5["destinations"])
	}
}

// TestAssembleTraceSteps_NoBid asserts the no-bid path is narrated honestly:
// still five steps, served=false, and the summary says it no-bid.
func TestAssembleTraceSteps_NoBid(t *testing.T) {
	fr := fireResult{TraceID: "trace-nobid", Served: false}
	// No events at all (nothing to poll on a no-bid).
	resp := assembleTraceSteps(fr, traceSnapshot{}, false, demoTraceFireRetries)

	if len(resp.Steps) != 5 {
		t.Fatalf("steps = %d, want 5", len(resp.Steps))
	}
	d2 := resp.Steps[1].Data.(map[string]any)
	if d2["served"] != false {
		t.Errorf("step2 served = %v, want false", d2["served"])
	}
	if d2["attempts"] != demoTraceFireRetries {
		t.Errorf("step2 attempts = %v, want %d", d2["attempts"], demoTraceFireRetries)
	}
	d3 := resp.Steps[2].Data.(map[string]any)
	if d3["served"] != false {
		t.Errorf("step3 served = %v, want false", d3["served"])
	}
	// The auction step must NOT claim a winner on a no-bid.
	if w, _ := d3["winner_dsp"].(string); w != "" {
		t.Errorf("step3 winner_dsp = %q, want empty on no-bid", w)
	}
	if !strings.Contains(resp.Summary, "no-bid") {
		t.Errorf("summary = %q, want it to mention no-bid", resp.Summary)
	}
}

// TestAssembleTraceSteps_ServedButEventsInFlight covers a served request whose
// events haven't reached reporting yet (poll timed out): served=true but no
// impression in the snapshot → step 4 narrates "in flight", summary flags it.
func TestAssembleTraceSteps_ServedButEventsInFlight(t *testing.T) {
	fr := fireResult{TraceID: "trace-inflight", Served: true}
	resp := assembleTraceSteps(fr, traceSnapshot{}, false, 1)

	d4 := resp.Steps[3].Data.(map[string]any)
	if d4["impression"] != false {
		t.Errorf("step4 impression = %v, want false", d4["impression"])
	}
	if !strings.Contains(resp.Summary, "in flight") {
		t.Errorf("summary = %q, want it to mention events in flight", resp.Summary)
	}
}

// TestTraceOrchestratorRun_RetriesNoBid asserts the run loop retries the fire up
// to demoTraceFireRetries times on a persistent no-bid, then reports it.
func TestTraceOrchestratorRun_RetriesNoBid(t *testing.T) {
	fb := &fakeTraceBackend{fire: fireResult{TraceID: "t1", Served: false}}
	o := &traceDemoOrchestrator{backend: fb, log: quietLog()}
	resp, err := o.run(context.Background())
	if err != nil {
		t.Fatalf("run err = %v", err)
	}
	if fb.fireN != demoTraceFireRetries {
		t.Errorf("fire attempts = %d, want %d", fb.fireN, demoTraceFireRetries)
	}
	if servedOf(resp) {
		t.Error("servedOf = true, want false on persistent no-bid")
	}
	// The run must cache so a subsequent GET replays it.
	got := o.currentState()
	if !got.Ran {
		t.Error("currentState Ran = false after a run, want true")
	}
}

// TestTraceOrchestratorRun_StopsOnFirstWin asserts a winning fire short-circuits
// the retry loop (fires exactly once).
func TestTraceOrchestratorRun_StopsOnFirstWin(t *testing.T) {
	fb := &fakeTraceBackend{fire: fireResult{TraceID: "t2", Served: true}, snap: syntheticWinTrace("t2"), ok: true}
	o := &traceDemoOrchestrator{backend: fb, log: quietLog()}
	if _, err := o.run(context.Background()); err != nil {
		t.Fatalf("run err = %v", err)
	}
	if fb.fireN != 1 {
		t.Errorf("fire attempts = %d, want 1 on immediate win", fb.fireN)
	}
}
