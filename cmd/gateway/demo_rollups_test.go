package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// fakeRollupsBackend drives the orchestrator in unit tests without a live stack
// or the reporting query API. It returns canned fire + count results so the
// pure step-assembly and the orchestrator wiring can be exercised deterministically.
type fakeRollupsBackend struct {
	fire   fireOutcome
	raw    countResult
	fine   countResult
	coarse countResult

	fireN    int
	rawN     int
	groupedN int
}

func (f *fakeRollupsBackend) fireImpressions(context.Context, int) fireOutcome {
	f.fireN++
	return f.fire
}

func (f *fakeRollupsBackend) rawCount(context.Context, time.Time) countResult {
	f.rawN++
	return f.raw
}

func (f *fakeRollupsBackend) groupedCount(_ context.Context, _ time.Time, dims []string) countResult {
	f.groupedN++
	// Return the fine result for the fine dims, coarse otherwise — so a single
	// fake covers both grouped queries the run makes.
	if len(dims) == len(demoRollupFineDims) {
		return f.fine
	}
	return f.coarse
}

// TestDemoRollupsHandler_Permissions mirrors the sibling demos' gating: staff
// read may VIEW, staff update may RUN; everyone else 403; the backend nil-check
// sits behind the permission gate (403 never leaks wiring).
func TestDemoRollupsHandler_Permissions(t *testing.T) {
	o := &rollupsDemoOrchestrator{backend: nil, log: quietLog()} // nil backend → 503 after gate
	h := demoRollupsHandler(o)

	req := func(method, target string, claims *auth.Claims) *http.Request {
		r := httptest.NewRequest(method, target, nil)
		if claims != nil {
			r = withClaims(r, claims)
		}
		return r
	}

	staffRead := &auth.Claims{AccountType: auth.AccountStaff, Permissions: []string{"support:read"}}
	staffUpdate := &auth.Claims{AccountType: auth.AccountStaff, Permissions: []string{"support:read", "support:update"}}
	advertiser := &auth.Claims{AccountType: auth.AccountAdvertiser, Permissions: []string{"billing:view"}}

	cases := []struct {
		name   string
		method string
		target string
		claims *auth.Claims
		want   int
	}{
		{"no claims → 401", http.MethodGet, routes.APIDemoRollups, nil, http.StatusUnauthorized},
		{"advertiser view → 403", http.MethodGet, routes.APIDemoRollups, advertiser, http.StatusForbidden},
		{"advertiser run → 403", http.MethodPost, routes.APIDemoRollupsRun, advertiser, http.StatusForbidden},
		// support:read may VIEW but NOT run (support:update required).
		{"read-only staff run → 403", http.MethodPost, routes.APIDemoRollupsRun, staffRead, http.StatusForbidden},
		// With the right permission, gating passes and we reach the nil-backend 503.
		{"staff run reaches backend → 503", http.MethodPost, routes.APIDemoRollupsRun, staffUpdate, http.StatusServiceUnavailable},
		{"unsupported method → 405", http.MethodDelete, routes.APIDemoRollups, staffUpdate, http.StatusMethodNotAllowed},
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

// TestDemoRollupsHandler_ViewBeforeRun asserts a staff GET before any run returns
// a valid "not run yet" shape (200, ran=false) rather than 503.
func TestDemoRollupsHandler_ViewBeforeRun(t *testing.T) {
	o := &rollupsDemoOrchestrator{backend: &fakeRollupsBackend{}, log: quietLog()}
	h := demoRollupsHandler(o)
	r := withClaims(httptest.NewRequest(http.MethodGet, routes.APIDemoRollups, nil),
		&auth.Claims{AccountType: auth.AccountStaff, Permissions: []string{"support:read"}})
	rec := httptest.NewRecorder()
	h(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"ran":false`) {
		t.Errorf("body = %s, want ran:false", rec.Body.String())
	}
}

// TestAssembleRollupSteps_Collapse covers the happy path: five steps, the ratio
// is raw/agg, totals match, and the coarser count is smaller.
func TestAssembleRollupSteps_Collapse(t *testing.T) {
	fire := fireOutcome{Fired: 8, Served: 8}
	raw := countResult{OK: true, Total: 76186, Rows: 1}
	// 1280 aggregated rows whose counts sum back to the raw total (same totals).
	fine := countResult{OK: true, Total: 76186, Rows: 1280}
	coarse := countResult{OK: true, Total: 76186, Rows: 240}
	resp := assembleRollupSteps(fire, raw, fine, coarse)

	if !resp.Ran {
		t.Fatal("Ran = false, want true")
	}
	if len(resp.Steps) != 5 {
		t.Fatalf("steps = %d, want 5", len(resp.Steps))
	}
	for i, s := range resp.Steps {
		if s.N != i+1 {
			t.Fatalf("step[%d].N = %d, want %d", i, s.N, i+1)
		}
	}

	// Step 1: firehose reports fired/served.
	d1 := resp.Steps[0].Data.(map[string]any)
	if d1["fired"] != 8 || d1["served"] != 8 {
		t.Errorf("step1 fire = %+v, want fired 8 served 8", d1)
	}

	// Step 2: the raw row count.
	d2 := resp.Steps[1].Data.(map[string]any)
	if got := d2["raw_rows"].(int64); got != 76186 {
		t.Errorf("step2 raw_rows = %d, want 76186", got)
	}

	// Step 3: the collapse — ratio == raw/agg, totals match.
	d3 := resp.Steps[2].Data.(map[string]any)
	if got := d3["aggregated_rows"].(int); got != 1280 {
		t.Errorf("step3 aggregated_rows = %d, want 1280", got)
	}
	if got := d3["reduction"].(float64); got != float64(76186)/float64(1280) {
		t.Errorf("step3 reduction = %v, want %v", got, float64(76186)/float64(1280))
	}
	if d3["totals_match"] != true {
		t.Errorf("step3 totals_match = %v, want true", d3["totals_match"])
	}

	// Step 4: coarser = fewer rows.
	d4 := resp.Steps[3].Data.(map[string]any)
	if got := d4["coarse_rows"].(int); got != 240 {
		t.Errorf("step4 coarse_rows = %d, want 240", got)
	}
	if d4["coarse_rows"].(int) >= d4["fine_rows"].(int) {
		t.Errorf("coarse_rows %v should be < fine_rows %v", d4["coarse_rows"], d4["fine_rows"])
	}

	// Step 5: the retention ladder is present.
	d5 := resp.Steps[4].Data.(map[string]any)
	if tiers, ok := d5["retention"].([]map[string]string); !ok || len(tiers) != 5 {
		t.Errorf("step5 retention = %v, want 5 tiers", d5["retention"])
	}

	if !strings.Contains(resp.Summary, "collapsed") {
		t.Errorf("summary = %q, want it to describe the collapse", resp.Summary)
	}
}

// TestAssembleRollupSteps_TotalsMismatch asserts an honest note when the grouped
// total drifts from the raw total (window shifted between the two queries).
func TestAssembleRollupSteps_TotalsMismatch(t *testing.T) {
	raw := countResult{OK: true, Total: 100, Rows: 1}
	fine := countResult{OK: true, Total: 108, Rows: 10} // 8 extra landed between queries
	coarse := countResult{OK: true, Total: 108, Rows: 3}
	resp := assembleRollupSteps(fireOutcome{Fired: 8, Served: 8}, raw, fine, coarse)

	d3 := resp.Steps[2].Data.(map[string]any)
	if d3["totals_match"] != false {
		t.Errorf("totals_match = %v, want false", d3["totals_match"])
	}
	if !strings.Contains(strings.ToLower(resp.Steps[2].Narration), "gap") &&
		!strings.Contains(strings.ToLower(resp.Steps[2].Narration), "window shifted") {
		t.Errorf("step3 narration = %q, want it to flag the shifted window honestly", resp.Steps[2].Narration)
	}
}

// TestAssembleRollupSteps_QueryFailed asserts a failed query is narrated
// honestly (no fabricated numbers) across the raw + grouped steps.
func TestAssembleRollupSteps_QueryFailed(t *testing.T) {
	resp := assembleRollupSteps(
		fireOutcome{Fired: 8, Served: 8},
		countResult{OK: false}, countResult{OK: false}, countResult{OK: false},
	)
	if len(resp.Steps) != 5 {
		t.Fatalf("steps = %d, want 5 even on failure", len(resp.Steps))
	}
	if resp.Steps[1].Data.(map[string]any)["query_ok"] != false {
		t.Errorf("step2 query_ok = %v, want false", resp.Steps[1].Data.(map[string]any)["query_ok"])
	}
	if !strings.Contains(strings.ToLower(resp.Steps[1].Narration), "failed") &&
		!strings.Contains(strings.ToLower(resp.Steps[1].Narration), "nothing") {
		t.Errorf("step2 narration = %q, want it to narrate the failure honestly", resp.Steps[1].Narration)
	}
	if strings.Contains(strings.ToLower(resp.Summary), "collapsed to") {
		t.Errorf("summary = %q, must not fabricate a collapse when queries failed", resp.Summary)
	}
}

// TestAssembleRollupSteps_NoServed asserts the no-served (no-bid) firehose is
// narrated honestly: step 1 says nothing won, the collapse still reads whatever
// is in the window.
func TestAssembleRollupSteps_NoServed(t *testing.T) {
	fire := fireOutcome{Fired: 8, Served: 0}
	raw := countResult{OK: true, Total: 500, Rows: 1}
	fine := countResult{OK: true, Total: 500, Rows: 50}
	coarse := countResult{OK: true, Total: 500, Rows: 10}
	resp := assembleRollupSteps(fire, raw, fine, coarse)

	if !strings.Contains(strings.ToLower(resp.Steps[0].Narration), "no-bid") &&
		!strings.Contains(strings.ToLower(resp.Steps[0].Narration), "none won") {
		t.Errorf("step1 narration = %q, want it to say nothing won", resp.Steps[0].Narration)
	}
	// The collapse still shows the pre-existing window's rows.
	d3 := resp.Steps[2].Data.(map[string]any)
	if got := d3["aggregated_rows"].(int); got != 50 {
		t.Errorf("step3 aggregated_rows = %d, want 50", got)
	}
}

// TestReductionRatio guards the divide-by-zero + basic ratio math.
func TestReductionRatio(t *testing.T) {
	if got := reductionRatio(76186, 1280); got != float64(76186)/float64(1280) {
		t.Errorf("reductionRatio(76186,1280) = %v", got)
	}
	if got := reductionRatio(100, 0); got != 0 {
		t.Errorf("reductionRatio(100,0) = %v, want 0 (no divide-by-zero)", got)
	}
}

// TestRollupsOrchestratorRun drives the full run through the fake: fire → wait →
// raw + two grouped queries → assemble → cache. The land-wait is shrunk so the
// test is fast.
func TestRollupsOrchestratorRun(t *testing.T) {
	old := demoRollupsLandWait
	demoRollupsLandWait = time.Millisecond
	defer func() { demoRollupsLandWait = old }()

	fb := &fakeRollupsBackend{
		fire:   fireOutcome{Fired: 8, Served: 8},
		raw:    countResult{OK: true, Total: 76186, Rows: 1},
		fine:   countResult{OK: true, Total: 76186, Rows: 1280},
		coarse: countResult{OK: true, Total: 76186, Rows: 240},
	}
	o := &rollupsDemoOrchestrator{backend: fb, log: quietLog()}
	resp, err := o.run(context.Background())
	if err != nil {
		t.Fatalf("run err = %v", err)
	}
	if fb.fireN != 1 {
		t.Errorf("fireImpressions calls = %d, want 1", fb.fireN)
	}
	if fb.rawN != 1 {
		t.Errorf("rawCount calls = %d, want 1", fb.rawN)
	}
	if fb.groupedN != 2 {
		t.Errorf("groupedCount calls = %d, want 2 (fine + coarse)", fb.groupedN)
	}
	d3 := resp.Steps[2].Data.(map[string]any)
	if d3["totals_match"] != true {
		t.Errorf("run step3 totals_match = %v, want true", d3["totals_match"])
	}
	if got := o.currentState(); !got.Ran {
		t.Error("currentState Ran = false after a run, want true")
	}
}

// TestFmtN checks the thousands-separator formatter used in the narration.
func TestFmtN(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0"}, {76186, "76,186"}, {1280, "1,280"}, {1000000, "1,000,000"}, {999, "999"},
	}
	for _, c := range cases {
		if got := fmtN(c.n); got != c.want {
			t.Errorf("fmtN(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}
