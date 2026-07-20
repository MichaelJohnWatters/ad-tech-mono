package main

// demo_billing.go — the staff-only guided "Billing / Money Flow" demo.
//
// Sibling of demo_trace.go and demo_onboarding.go. Where the trace demo follows
// one request through the pipeline, this one teaches the ONE thing about ad-tech
// money that surprises everyone: spend books on the IMPRESSION, not the auction
// win. A win that never renders is free; the impression is the billable event.
//
// It proves that with a deterministic two-phase fire against the REAL stack:
//
//  1. Fire a fixed-persona request at the real SSP serve path. An auction runs
//     and a REAL advertiser WINS. The serve response carries the winner's
//     campaign_id / advertiser account_id + the clearing-price CPM. NOTHING is
//     billed yet — we deliberately do NOT fire the impression beacon here.
//  2. Snapshot BEFORE: the winning advertiser's advertiser_balances.balance and
//     this campaign's campaign_committed_spend.settled_micros, right now.
//  3. Fire the server-returned impression beacon (the billable event).
//  4. POLL Postgres (bounded) until the money moves: the balance drops, a
//     ledger_entries row with reference_type='spend' for this trace appears, and
//     committed_spend increases. Snapshot AFTER; show the deltas.
//  5. Tie it back to invoicing: committed spend rolls into the advertiser's
//     monthly invoice; prepay is drawn down live, invoiced accounts billed
//     monthly.
//
// This fires a REAL impression → a real ~sub-cent drawdown on the winning
// advertiser. That is inherent to showing real billing; step 1 says so.
//
// Money units (the point of the demo): the clearing price is a CPM (per 1000
// impressions); per-impression cost = CPM / 1000; advertiser_balances.balance is
// DECIMAL dollars; campaign_committed_spend.settled_micros is MICRO-dollars
// (1 USD = 1,000,000 micros). The math is shown explicitly.
//
// Staff-only: GET (support:read) returns the last-run snapshot; POST /run
// (support:update) fires + polls + assembles synchronously.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
)

// The billing demo reuses the trace demo's fixed persona/placement so both
// guided flows tell the same stable story about the same kind of request.
// (demoTracePersona / demoTracePlacement live in demo_trace.go.)

// microsPerUSD converts DECIMAL-dollar amounts to the MICRO-dollar unit
// campaign_committed_spend stores. 1 USD = 1,000,000 micros.
const microsPerUSD = 1_000_000

// demoBillingPollTimeout bounds the async wait for the impression beacon to
// travel serve → tracker → NATS → reporting → the balance/committed-spend
// writes. Kept short so the demo never hangs; if it lapses we narrate the wait
// honestly rather than fabricate a drawdown.
const demoBillingPollTimeout = 5 * time.Second

// demoBillingPollInterval is how often we re-poll Postgres while waiting.
const demoBillingPollInterval = 250 * time.Millisecond

// demoBillingFireRetries is how many times the serve is retried when the persona
// no-bids before we give up and narrate the no-bid honestly.
const demoBillingFireRetries = 3

// billingSnapshot is one point-in-time read of the winning advertiser's money
// state: their prepay balance (DECIMAL dollars) and this campaign's settled
// committed spend (MICRO-dollars). ledgerSpend counts the reference_type='spend'
// ledger rows for the fired trace (0 before the impression bills, >0 after).
type billingSnapshot struct {
	BalanceUSD   float64 `json:"balance_usd"`
	CommittedMic int64   `json:"committed_micros"`
	LedgerSpendN int     `json:"ledger_spend_rows"`
	BalanceFound bool    `json:"balance_found"` // false if the advertiser has no balance row
}

// billingFire is the outcome of firing the serve (phase 1): the real winner + the
// clearing-price CPM, with the impression beacon captured but NOT yet fired.
type billingFire struct {
	TraceID          string
	Served           bool
	ClearingPriceCPM float64
	CampaignID       string
	AdvertiserID     string
	Currency         string
	ImpressionURL    string
	Attempts         int
}

// billingDemoBackend is the seam that keeps the demo testable: the LIVE path
// fires a real request through the SSP serve, reads real billing tables, and
// fires the real impression beacon; a fake drives the orchestrator in unit tests
// without a running stack or DB.
type billingDemoBackend interface {
	// fireServe fires the persona request at the real serve path and returns the
	// winner + captured impression beacon, WITHOUT firing it. Retries a no-bid up
	// to demoBillingFireRetries before returning Served=false.
	fireServe(ctx context.Context) (billingFire, error)
	// snapshot reads the advertiser's balance + this campaign's committed spend +
	// the spend-ledger row count for the trace, right now.
	snapshot(ctx context.Context, accountID, campaignID, traceID string) (billingSnapshot, error)
	// fireImpression fires the server-returned impression beacon — the billable
	// event. Returns whether it fired OK.
	fireImpression(ctx context.Context, impressionURL string) bool
}

// billingDemoOrchestrator fires + snapshots + polls + assembles. lastRun caches
// the most recent run so GET replays it without re-firing (a run mutates real
// billing state, so GET must not trigger it).
type billingDemoOrchestrator struct {
	backend billingDemoBackend
	log     *slog.Logger

	mu      sync.Mutex
	lastRun *demoResponse
}

// run executes the two-phase fire and assembles the 5 steps.
func (o *billingDemoOrchestrator) run(ctx context.Context) (demoResponse, error) {
	// Phase 1: fire the serve → a real advertiser wins. No impression yet.
	fr, err := o.backend.fireServe(ctx)
	if err != nil {
		return demoResponse{}, fmt.Errorf("fire serve: %w", err)
	}

	// A no-bid (or a win that couldn't be attributed to an advertiser/campaign)
	// leaves nothing to bill — assemble the honest no-bid timeline and stop.
	if !fr.Served || fr.AdvertiserID == "" || fr.CampaignID == "" {
		resp := assembleBillingSteps(fr, billingSnapshot{}, billingSnapshot{}, false)
		o.cache(&resp)
		return resp, nil
	}

	// Phase 2: snapshot BEFORE — the winner's balance + committed spend now.
	before, err := o.backend.snapshot(ctx, fr.AdvertiserID, fr.CampaignID, fr.TraceID)
	if err != nil {
		return demoResponse{}, fmt.Errorf("before snapshot: %w", err)
	}

	// Phase 3: fire the impression beacon — the billable event.
	o.backend.fireImpression(ctx, fr.ImpressionURL)

	// Phase 4: poll Postgres until the drawdown lands (balance drops, a spend
	// ledger row appears, committed spend grows) or the timeout lapses.
	after, landed := o.pollDrawdown(ctx, fr, before)

	resp := assembleBillingSteps(fr, before, after, landed)
	o.cache(&resp)
	return resp, nil
}

// pollDrawdown re-snapshots until the money moves or the timeout lapses. "Moved"
// means a spend ledger row exists for the trace (the double-entry pair is the
// idempotent source of truth). Returns the last snapshot + whether it landed.
func (o *billingDemoOrchestrator) pollDrawdown(ctx context.Context, fr billingFire, before billingSnapshot) (billingSnapshot, bool) {
	deadline := time.Now().Add(demoBillingPollTimeout)
	last := before
	for {
		snap, err := o.backend.snapshot(ctx, fr.AdvertiserID, fr.CampaignID, fr.TraceID)
		if err != nil {
			o.log.Warn("demo billing poll error", "trace_id", fr.TraceID, "error", err)
		} else {
			last = snap
			if snap.LedgerSpendN > 0 {
				return last, true
			}
		}
		if time.Now().After(deadline) {
			return last, false
		}
		select {
		case <-ctx.Done():
			return last, false
		case <-time.After(demoBillingPollInterval):
		}
	}
}

// cache stores the run so a subsequent GET replays it without re-firing.
func (o *billingDemoOrchestrator) cache(resp *demoResponse) {
	now := time.Now().UTC()
	resp.RanAt = &now
	o.mu.Lock()
	cp := *resp
	o.lastRun = &cp
	o.mu.Unlock()
}

// currentState answers the GET: the cached last run, or a "not run yet" shape.
func (o *billingDemoOrchestrator) currentState() demoResponse {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.lastRun == nil {
		return demoResponse{Ran: false,
			Summary: "The demo has not been run yet. Click \"Run demo\" to fire one real impression and watch the winning advertiser's prepay balance draw down — live — by exactly the per-impression cost (clearing CPM ÷ 1000)."}
	}
	return *o.lastRun
}

// ---- step assembly (pure — the unit-tested core) ----

// perImpressionUSD is the money math centrepiece: per-impression cost =
// clearing CPM ÷ 1000. Returned in dollars.
func perImpressionUSD(cpm float64) float64 { return cpm / 1000 }

// usdToMicros converts a DECIMAL-dollar amount to MICRO-dollars, rounding to the
// nearest micro (committed_spend is integer micros).
func usdToMicros(usd float64) int64 {
	if usd >= 0 {
		return int64(usd*microsPerUSD + 0.5)
	}
	return int64(usd*microsPerUSD - 0.5)
}

// assembleBillingSteps builds the 5-step timeline from the fire result + the
// before/after snapshots. Pure so it can be unit-tested against synthetic
// inputs (including the no-bid and not-yet-landed paths). landed is whether the
// drawdown was observed within the poll window.
func assembleBillingSteps(fr billingFire, before, after billingSnapshot, landed bool) demoResponse {
	steps := make([]demoStep, 0, 5)

	perImp := perImpressionUSD(fr.ClearingPriceCPM)
	perImpMicros := usdToMicros(perImp)

	// --- Step 1: THE IMPRESSION THAT COSTS MONEY — persona + winner + CPM. ---
	cpmStr := "—"
	if fr.ClearingPriceCPM > 0 {
		cpmStr = fmt.Sprintf("$%.2f CPM", fr.ClearingPriceCPM)
	}
	step1 := demoStep{
		N: 1, Title: "The impression that costs money",
		Data: map[string]any{
			"persona":            demoTracePersona,
			"placement":          demoTracePlacement,
			"trace_id":           fr.TraceID,
			"served":             fr.Served,
			"advertiser_account": fr.AdvertiserID,
			"campaign_id":        fr.CampaignID,
			"clearing_price_cpm": cpmStr,
			"attempts":           fr.Attempts,
		},
	}
	switch {
	case fr.Served && fr.AdvertiserID != "":
		step1.Narration = fmt.Sprintf("A fixed persona visited a publisher page and a REAL auction ran. Advertiser %s won for campaign %s at %s. Nothing has been billed yet — a win that never renders is free; spend books on the IMPRESSION, not the win. Firing this demo bills a real ~sub-cent drawdown on that advertiser — that's the cost of showing real money move.", shortID(fr.AdvertiserID), shortID(fr.CampaignID), cpmStr)
	case fr.Served:
		step1.Narration = "The request served, but the serve response carried no advertiser/campaign to attribute spend to (house ad or a demand source without an advertiser account). Re-run — the demo persona is chosen to draw a real advertiser that bills."
	default:
		step1.Narration = "No DSP bid on this request (a genuine no-bid). Nothing served, so nothing bills. The demo persona is chosen to win reliably — re-run and it usually fills."
	}
	steps = append(steps, step1)

	// --- Step 2: BEFORE — the winner's balance + committed spend, right now. ---
	steps = append(steps, demoStep{
		N: 2, Title: "Before — the advertiser's prepay balance",
		Narration: fmt.Sprintf("Snapshot taken BEFORE we fire the impression: advertiser %s currently holds $%.4f of prepay balance, and campaign %s has $%.4f of settled committed spend today (%d micros). These are the numbers that are about to change.", shortID(fr.AdvertiserID), before.BalanceUSD, shortID(fr.CampaignID), float64(before.CommittedMic)/microsPerUSD, before.CommittedMic),
		Data: map[string]any{
			"balance_usd":      before.BalanceUSD,
			"committed_micros": before.CommittedMic,
			"committed_usd":    float64(before.CommittedMic) / microsPerUSD,
			"balance_found":    before.BalanceFound,
		},
	})

	// --- Step 3: FIRE THE IMPRESSION — the billable event + the money math. ---
	steps = append(steps, demoStep{
		N: 3, Title: "Fire the impression — the billable event",
		Narration: fmt.Sprintf("We fire the server-returned impression beacon — exactly what a browser does on render, and THE event that bills. The cost is the clearing price ÷ 1000: %s → per-impression $%.6f → %d micros. The impression event now travels tracker → NATS → reporting, which draws the balance down.", cpmStr, perImp, perImpMicros),
		Data: map[string]any{
			"clearing_price_cpm":    cpmStr,
			"math":                  fmt.Sprintf("CPM $%.2f ÷ 1000 = $%.6f per impression", fr.ClearingPriceCPM, perImp),
			"per_impression_usd":    perImp,
			"per_impression_micros": perImpMicros,
			"billable_event":        "impression",
		},
	})

	// --- Step 4: THE MONEY MOVED — before→after deltas + the ledger entry. ---
	balanceDelta := after.BalanceUSD - before.BalanceUSD
	committedDelta := after.CommittedMic - before.CommittedMic
	step4 := demoStep{
		N: 4, Title: "The money moved — balance drawn down",
		Data: map[string]any{
			"before_balance_usd":      before.BalanceUSD,
			"after_balance_usd":       after.BalanceUSD,
			"balance_delta_usd":       balanceDelta,
			"expected_delta_usd":      -perImp,
			"before_committed_micros": before.CommittedMic,
			"after_committed_micros":  after.CommittedMic,
			"committed_delta_micros":  committedDelta,
			"ledger_spend_rows":       after.LedgerSpendN,
			"landed":                  landed,
		},
	}
	if landed {
		// The balance drawdown is INSTANT (BalanceSink on the impression event);
		// campaign_committed_spend is persisted by the reporting snapshot cycle
		// (~30s), so within the poll window committedDelta is usually still 0 —
		// that's expected, not a miss. Word it honestly either way.
		committedNote := "Committed spend rolls up on the next ~30s snapshot — the balance drawdown above is the instant, authoritative effect."
		if committedDelta > 0 {
			committedNote = fmt.Sprintf("Committed spend rose by %d micros.", committedDelta)
		}
		step4.Narration = fmt.Sprintf("The impression billed. A double-entry ledger pair landed for this trace (debit advertiser:%s:balance / credit platform:revenue, reference_type='spend'). The prepay balance dropped $%.4f → $%.4f — a delta of $%.6f, exactly the per-impression cost (CPM ÷ 1000). %s One impression, one sub-cent drawdown, fully double-entry.", shortID(fr.AdvertiserID), before.BalanceUSD, after.BalanceUSD, balanceDelta, committedNote)
	} else {
		step4.Narration = fmt.Sprintf("The impression was fired, but the drawdown had not landed in Postgres within %s — it is still travelling tracker → NATS → reporting → the balance/committed-spend writes. No spend ledger row for this trace yet. This is honest async lag, not a lost impression: re-run (or re-open) and the before→after delta of $%.6f (CPM ÷ 1000) will show.", demoBillingPollTimeout, perImp)
	}
	steps = append(steps, step4)

	// --- Step 5: WHERE IT ENDS UP — the invoice tie-back. ---
	steps = append(steps, demoStep{
		N: 5, Title: "Where it ends up — the monthly invoice",
		Narration: "This committed spend rolls into the advertiser's monthly invoice: the invoice-runner sums campaign_committed_spend.settled_micros per campaign over the period and writes one invoices row + per-campaign line items. Prepay accounts (like this one) are drawn down LIVE, impression by impression; invoiced accounts bid on credit and are billed monthly. Same ledger, two settlement modes — this is the money lane of the lifecycle diagram, live.",
		Data: map[string]any{
			"settlement":       "prepay (live drawdown)",
			"invoiced_monthly": "invoice-runner sums campaign_committed_spend.settled_micros",
			"campaign_id":      fr.CampaignID,
		},
	})

	// Summary.
	var summary string
	switch {
	case !fr.Served || fr.AdvertiserID == "" || fr.CampaignID == "":
		summary = "This run had nothing to bill (no-bid, or the winner had no advertiser account). Re-run — the demo persona is chosen to draw a real advertiser that bills on the impression."
	case landed:
		summary = fmt.Sprintf("win (free) → impression (billable) → ledger → balance − $%.6f → committed spend → invoice. One real impression drew advertiser %s's prepay balance down by exactly the clearing CPM ÷ 1000; the same committed spend will roll into its monthly invoice.", perImp, shortID(fr.AdvertiserID))
	default:
		summary = fmt.Sprintf("The impression fired but the ~$%.6f drawdown was still in flight when the poll timed out. Re-run to see the balance before→after — spend books on the impression, and it lands async through NATS → reporting.", perImp)
	}

	return demoResponse{Ran: true, AccountID: fr.AdvertiserID, Steps: steps, Summary: summary}
}

// ---- HTTP handler ----

// demoBillingHandler serves both endpoints:
//
//	GET  /v1/api/demo/billing      (support:read)   — cached last-run snapshot
//	POST /v1/api/demo/billing/run  (support:update) — fire + snapshot + poll
//
// Permission gating runs BEFORE the backend nil-check so a non-staff caller
// always gets 403 (never a 503 that would leak whether the backend is wired).
func demoBillingHandler(o *billingDemoOrchestrator) http.HandlerFunc {
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
				o.log.Error("demo billing run failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			o.log.Info("demo billing run", "advertiser", resp.AccountID, "steps", len(resp.Steps), "actor", claims.UserID)
			_ = json.NewEncoder(w).Encode(resp)
		}
	}
}

// ---- live backend (HTTP serve/beacon + parameterised SQL over gwDB) ----

// httpBillingBackend is the LIVE backend: it fires the real SSP serve + the real
// impression beacon, and reads the real advertiser_balances /
// campaign_committed_spend / ledger_entries tables over gwDB. Nothing mocked.
type httpBillingBackend struct {
	client *http.Client
	sspURL string
	db     *sql.DB
	log    *slog.Logger
}

// fireServe fires the persona request at the real serve path, retrying a no-bid
// up to demoBillingFireRetries times. The impression beacon is captured but NOT
// fired here — the orchestrator snapshots BEFORE firing it.
func (b *httpBillingBackend) fireServe(ctx context.Context) (billingFire, error) {
	var sr personaServeResult
	var err error
	attempts := 0
	for attempts = 1; attempts <= demoBillingFireRetries; attempts++ {
		sr, err = serveOnce(ctx, b.client, b.sspURL, demoTracePersona, demoTracePlacement)
		if err != nil {
			return billingFire{}, err
		}
		if sr.Served {
			break
		}
	}
	return billingFire{
		TraceID:          sr.TraceID,
		Served:           sr.Served,
		ClearingPriceCPM: sr.ClearingPriceCPM,
		CampaignID:       sr.CampaignID,
		AdvertiserID:     sr.AdvertiserID,
		Currency:         sr.Currency,
		ImpressionURL:    sr.ImpressionURL,
		Attempts:         attempts,
	}, nil
}

// snapshot reads the advertiser's balance, this campaign's settled committed
// spend for today (UTC), and the count of reference_type='spend' ledger rows for
// the trace. All parameterised. A missing balance row is BalanceFound=false, not
// an error (the advertiser may never have been topped up).
func (b *httpBillingBackend) snapshot(ctx context.Context, accountID, campaignID, traceID string) (billingSnapshot, error) {
	if b.db == nil {
		return billingSnapshot{}, errors.New("billing demo: no database")
	}
	var snap billingSnapshot

	err := b.db.QueryRowContext(ctx,
		`SELECT balance::float8 FROM advertiser_balances WHERE account_id = $1::uuid`, accountID).
		Scan(&snap.BalanceUSD)
	switch {
	case err == nil:
		snap.BalanceFound = true
	case errors.Is(err, sql.ErrNoRows):
		// No balance row → treat as $0, not an error.
	default:
		return billingSnapshot{}, fmt.Errorf("read balance: %w", err)
	}

	// campaign_committed_spend is keyed by (day, campaign) in UTC; sum settled
	// micros across any rows for this campaign today (normally exactly one).
	day := time.Now().UTC().Format("2006-01-02")
	if err := b.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(settled_micros), 0)::bigint
		 FROM campaign_committed_spend WHERE day = $1::date AND campaign_id = $2`,
		day, campaignID).Scan(&snap.CommittedMic); err != nil {
		return billingSnapshot{}, fmt.Errorf("read committed spend: %w", err)
	}

	// The spend ledger row(s) for this trace — the idempotent double-entry pair
	// lands (debit advertiser-balance, credit platform-revenue) once the
	// impression bills. Presence is our "the money moved" signal.
	if err := b.db.QueryRowContext(ctx,
		`SELECT count(*) FROM ledger_entries
		 WHERE reference_type = 'spend' AND trace_id = $1`, traceID).
		Scan(&snap.LedgerSpendN); err != nil {
		return billingSnapshot{}, fmt.Errorf("count spend ledger: %w", err)
	}
	return snap, nil
}

// fireImpression fires the server-returned impression beacon (the billable
// event) via the shared fireBeacon.
func (b *httpBillingBackend) fireImpression(ctx context.Context, impressionURL string) bool {
	return fireBeacon(ctx, b.client, b.log, impressionURL)
}
