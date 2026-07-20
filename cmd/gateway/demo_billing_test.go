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

// fakeBillingBackend drives the orchestrator in unit tests without a live stack
// or DB. It serves a synthetic winner, and returns the BEFORE snapshot until the
// impression fires, then the AFTER snapshot — so pollDrawdown observes the money
// move exactly like the live path.
type fakeBillingBackend struct {
	fire       billingFire
	before     billingSnapshot
	after      billingSnapshot
	fired      bool // fireImpression flips this; snapshot then returns `after`
	fireServeN int
	snapshotN  int
	fireImpN   int
	neverLands bool // when true, keep returning `before` even after firing
}

func (f *fakeBillingBackend) fireServe(context.Context) (billingFire, error) {
	f.fireServeN++
	return f.fire, nil
}

func (f *fakeBillingBackend) snapshot(context.Context, string, string, string) (billingSnapshot, error) {
	f.snapshotN++
	if f.fired && !f.neverLands {
		return f.after, nil
	}
	return f.before, nil
}

func (f *fakeBillingBackend) fireImpression(context.Context, string) bool {
	f.fireImpN++
	f.fired = true
	return true
}

// TestDemoBillingHandler_Permissions mirrors the trace/onboarding gating: staff
// read may VIEW, staff update may RUN; everyone else 403; the backend nil-check
// sits behind the permission gate (403 never leaks wiring).
func TestDemoBillingHandler_Permissions(t *testing.T) {
	o := &billingDemoOrchestrator{backend: nil, log: quietLog()} // nil backend → 503 after gate
	h := demoBillingHandler(o)

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
		{"no claims → 401", http.MethodGet, routes.APIDemoBilling, nil, http.StatusUnauthorized},
		{"advertiser view → 403", http.MethodGet, routes.APIDemoBilling, advertiser, http.StatusForbidden},
		{"advertiser run → 403", http.MethodPost, routes.APIDemoBillingRun, advertiser, http.StatusForbidden},
		// support:read may VIEW but NOT run (support:update required).
		{"read-only staff run → 403", http.MethodPost, routes.APIDemoBillingRun, staffRead, http.StatusForbidden},
		// With the right permission, gating passes and we reach the nil-backend 503.
		{"staff run reaches backend → 503", http.MethodPost, routes.APIDemoBillingRun, staffUpdate, http.StatusServiceUnavailable},
		{"unsupported method → 405", http.MethodDelete, routes.APIDemoBilling, staffUpdate, http.StatusMethodNotAllowed},
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

// TestDemoBillingHandler_ViewBeforeRun asserts a staff GET before any run returns
// a valid "not run yet" shape (200, ran=false) rather than 503.
func TestDemoBillingHandler_ViewBeforeRun(t *testing.T) {
	o := &billingDemoOrchestrator{backend: &fakeBillingBackend{}, log: quietLog()}
	h := demoBillingHandler(o)
	r := withClaims(httptest.NewRequest(http.MethodGet, routes.APIDemoBilling, nil),
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

// syntheticWinFire is a winning serve: CPM $2.50 → per-impression $0.0025.
func syntheticWinFire() billingFire {
	return billingFire{
		TraceID: "trace-bill", Served: true, ClearingPriceCPM: 2.50,
		CampaignID: "camp-1", AdvertiserID: "adv-1", Currency: "USD",
		ImpressionURL: "http://tracker/imp?sig=x", Attempts: 1,
	}
}

// TestAssembleBillingSteps_Drawdown covers the happy path: five steps, the delta
// equals CPM/1000 converted to micros, the ledger row landed, committed grew.
func TestAssembleBillingSteps_Drawdown(t *testing.T) {
	fr := syntheticWinFire()
	// CPM 2.50 → per-impression $0.0025 → 2500 micros.
	before := billingSnapshot{BalanceUSD: 10.0000, CommittedMic: 5000, BalanceFound: true}
	after := billingSnapshot{BalanceUSD: 9.9975, CommittedMic: 7500, LedgerSpendN: 1, BalanceFound: true}
	resp := assembleBillingSteps(fr, before, after, true)

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

	// Step 1 carries the real winner + CPM.
	d1 := resp.Steps[0].Data.(map[string]any)
	if d1["advertiser_account"] != "adv-1" || d1["campaign_id"] != "camp-1" {
		t.Errorf("step1 winner = %+v, want adv-1/camp-1", d1)
	}
	if d1["clearing_price_cpm"] != "$2.50 CPM" {
		t.Errorf("step1 cpm = %v, want $2.50 CPM", d1["clearing_price_cpm"])
	}

	// Step 3 shows the money math: CPM/1000 → per-impression micros.
	d3 := resp.Steps[2].Data.(map[string]any)
	if got := d3["per_impression_usd"].(float64); got != 0.0025 {
		t.Errorf("step3 per_impression_usd = %v, want 0.0025", got)
	}
	if got := d3["per_impression_micros"].(int64); got != 2500 {
		t.Errorf("step3 per_impression_micros = %v, want 2500", got)
	}

	// Step 4 is the centrepiece: the balance delta == −per-impression cost, and
	// the expected-delta matches, committed grew, ledger row present.
	d4 := resp.Steps[3].Data.(map[string]any)
	gotDelta := d4["balance_delta_usd"].(float64)
	if diff := gotDelta - (-0.0025); diff > 1e-9 || diff < -1e-9 {
		t.Errorf("step4 balance_delta_usd = %v, want -0.0025", gotDelta)
	}
	if exp := d4["expected_delta_usd"].(float64); exp != -0.0025 {
		t.Errorf("step4 expected_delta_usd = %v, want -0.0025", exp)
	}
	if d4["landed"] != true {
		t.Errorf("step4 landed = %v, want true", d4["landed"])
	}
	if got := d4["committed_delta_micros"].(int64); got != 2500 {
		t.Errorf("step4 committed_delta_micros = %v, want 2500", got)
	}
	if got := d4["ledger_spend_rows"].(int); got != 1 {
		t.Errorf("step4 ledger_spend_rows = %v, want 1", got)
	}

	if !strings.Contains(resp.Summary, "balance down") && !strings.Contains(resp.Summary, "balance −") && !strings.Contains(resp.Summary, "balance -") {
		// summary uses "balance − $..." — accept either minus glyph.
		if !strings.Contains(resp.Summary, "impression") {
			t.Errorf("summary = %q, want it to describe the drawdown", resp.Summary)
		}
	}
}

// TestAssembleBillingSteps_NoBid asserts the no-bid path is narrated honestly:
// still five steps, and the summary/step1 say it no-bid (nothing to bill).
func TestAssembleBillingSteps_NoBid(t *testing.T) {
	fr := billingFire{TraceID: "trace-nobid", Served: false, Attempts: demoBillingFireRetries}
	resp := assembleBillingSteps(fr, billingSnapshot{}, billingSnapshot{}, false)

	if len(resp.Steps) != 5 {
		t.Fatalf("steps = %d, want 5", len(resp.Steps))
	}
	d1 := resp.Steps[0].Data.(map[string]any)
	if d1["served"] != false {
		t.Errorf("step1 served = %v, want false", d1["served"])
	}
	if !strings.Contains(strings.ToLower(resp.Steps[0].Narration), "no-bid") {
		t.Errorf("step1 narration = %q, want it to mention no-bid", resp.Steps[0].Narration)
	}
	if !strings.Contains(strings.ToLower(resp.Summary), "no-bid") && !strings.Contains(strings.ToLower(resp.Summary), "nothing to bill") {
		t.Errorf("summary = %q, want it to mention the no-bid honestly", resp.Summary)
	}
}

// TestAssembleBillingSteps_NotYetLanded asserts a served/won impression whose
// drawdown hasn't landed in the poll window narrates the async wait honestly:
// landed=false, no delta yet, summary flags "in flight".
func TestAssembleBillingSteps_NotYetLanded(t *testing.T) {
	fr := syntheticWinFire()
	before := billingSnapshot{BalanceUSD: 10.0, CommittedMic: 5000, BalanceFound: true}
	// after == before, no ledger row: nothing moved within the window.
	after := billingSnapshot{BalanceUSD: 10.0, CommittedMic: 5000, LedgerSpendN: 0, BalanceFound: true}
	resp := assembleBillingSteps(fr, before, after, false)

	d4 := resp.Steps[3].Data.(map[string]any)
	if d4["landed"] != false {
		t.Errorf("step4 landed = %v, want false", d4["landed"])
	}
	if got := d4["balance_delta_usd"].(float64); got != 0 {
		t.Errorf("step4 balance_delta_usd = %v, want 0 (not landed)", got)
	}
	if !strings.Contains(strings.ToLower(resp.Summary), "in flight") {
		t.Errorf("summary = %q, want it to mention events in flight", resp.Summary)
	}
	// The math is still shown — teaching value survives the async lag.
	if !strings.Contains(resp.Steps[2].Narration, "1000") {
		t.Errorf("step3 narration = %q, want the CPM ÷ 1000 math", resp.Steps[2].Narration)
	}
}

// TestBillingOrchestratorRun_TwoPhase drives the full two-phase fire through the
// fake: serve (win) → BEFORE snapshot → fire impression → poll observes AFTER.
// The run must cache so a GET replays it.
func TestBillingOrchestratorRun_TwoPhase(t *testing.T) {
	fb := &fakeBillingBackend{
		fire:   syntheticWinFire(),
		before: billingSnapshot{BalanceUSD: 10.0, CommittedMic: 5000, BalanceFound: true},
		after:  billingSnapshot{BalanceUSD: 9.9975, CommittedMic: 7500, LedgerSpendN: 1, BalanceFound: true},
	}
	o := &billingDemoOrchestrator{backend: fb, log: quietLog()}
	resp, err := o.run(context.Background())
	if err != nil {
		t.Fatalf("run err = %v", err)
	}
	if fb.fireImpN != 1 {
		t.Errorf("fireImpression calls = %d, want 1", fb.fireImpN)
	}
	// BEFORE snapshot must be read before the impression fires (snapshotN >= 2:
	// one pre-fire + at least one poll).
	if fb.snapshotN < 2 {
		t.Errorf("snapshot calls = %d, want >= 2 (before + poll)", fb.snapshotN)
	}
	d4 := resp.Steps[3].Data.(map[string]any)
	if d4["landed"] != true {
		t.Errorf("run step4 landed = %v, want true", d4["landed"])
	}
	if got := o.currentState(); !got.Ran {
		t.Error("currentState Ran = false after a run, want true")
	}
}

// TestBillingOrchestratorRun_NoBidSkipsSnapshot asserts a no-bid short-circuits:
// no BEFORE snapshot, no impression fired, honest no-bid timeline.
func TestBillingOrchestratorRun_NoBidSkipsSnapshot(t *testing.T) {
	fb := &fakeBillingBackend{fire: billingFire{TraceID: "t", Served: false, Attempts: demoBillingFireRetries}}
	o := &billingDemoOrchestrator{backend: fb, log: quietLog()}
	if _, err := o.run(context.Background()); err != nil {
		t.Fatalf("run err = %v", err)
	}
	if fb.snapshotN != 0 {
		t.Errorf("snapshot calls = %d, want 0 on no-bid", fb.snapshotN)
	}
	if fb.fireImpN != 0 {
		t.Errorf("fireImpression calls = %d, want 0 on no-bid", fb.fireImpN)
	}
}

// TestUsdToMicros checks the money-unit conversion the demo hinges on.
func TestUsdToMicros(t *testing.T) {
	cases := []struct {
		usd  float64
		want int64
	}{
		{0.0025, 2500}, {0.000001, 1}, {1.0, 1_000_000}, {0, 0}, {-0.0025, -2500},
	}
	for _, c := range cases {
		if got := usdToMicros(c.usd); got != c.want {
			t.Errorf("usdToMicros(%v) = %d, want %d", c.usd, got, c.want)
		}
	}
	if got := perImpressionUSD(2.50); got != 0.0025 {
		t.Errorf("perImpressionUSD(2.50) = %v, want 0.0025", got)
	}
}
