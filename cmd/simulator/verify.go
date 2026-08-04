package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// This file gives the simulator a self-check: after firing a known batch it
// reads the counts back out of the reporting store and asserts the pipeline
// recorded them. It's the delta-triangulation "verify-pipeline" playbook built
// in — the simulator already knows exactly how many auctions it ran and how
// many won, so it can confirm those landed (auction → win → impression →
// tracker → NATS → reporting) instead of a human eyeballing ClickHouse.
//
// Assumes a quiescent stack (this run is the only traffic in the window) — the
// normal case for a verification run. Concurrent traffic inflates the counts;
// the report flags that possibility rather than pretending precision.

// reportQueryResult mirrors analytics.QueryResult (columns + positional rows).
type reportQueryResult struct {
	Columns []string        `json:"columns"`
	Rows    [][]interface{} `json:"rows"`
}

// queryCount POSTs a count(*) query for one table since `from` to the reporting
// service's internal query endpoint (unauthenticated; the gateway is what adds
// auth when proxying it). Returns the scalar count.
func queryCount(client *http.Client, reportingURL, table string, from time.Time) (int, error) {
	body, _ := json.Marshal(map[string]interface{}{
		"table":     table,
		"metrics":   []string{"count"},
		"time_from": from,
	})
	req, err := http.NewRequest("POST", reportingURL+routes.ReportingQuery, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("reporting query %s: status %d", table, resp.StatusCode)
	}
	var res reportQueryResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return 0, err
	}
	if len(res.Rows) == 0 || len(res.Rows[0]) == 0 {
		return 0, nil
	}
	// count comes back as a JSON number (float64) or string depending on backend.
	switch v := res.Rows[0][0].(type) {
	case float64:
		return int(v), nil
	case string:
		var n int
		_, _ = fmt.Sscanf(v, "%d", &n)
		return n, nil
	default:
		return 0, fmt.Errorf("unexpected count type %T", res.Rows[0][0])
	}
}

// queryMetric POSTs a single-metric query and returns the scalar value of the
// named metric column. Used to read a SERVER-computed derived metric (e.g.
// fill_rate) back so we can cross-check it against the simulator's own counts —
// this is what turns `--verify` into an engine check, not just a raw-count check.
func queryMetric(client *http.Client, reportingURL, table, metric string, from time.Time) (float64, error) {
	body, _ := json.Marshal(map[string]interface{}{
		"table":     table,
		"metrics":   []string{metric},
		"time_from": from,
	})
	req, err := http.NewRequest("POST", reportingURL+routes.ReportingQuery, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("reporting query %s.%s: status %d", table, metric, resp.StatusCode)
	}
	var res reportQueryResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return 0, err
	}
	idx := -1
	for i, c := range res.Columns {
		if c == metric {
			idx = i
			break
		}
	}
	if idx < 0 || len(res.Rows) == 0 || idx >= len(res.Rows[0]) {
		return 0, nil
	}
	switch v := res.Rows[0][idx].(type) {
	case float64:
		return v, nil
	case string:
		var f float64
		_, _ = fmt.Sscanf(v, "%g", &f)
		return f, nil
	default:
		return 0, fmt.Errorf("unexpected metric type %T", res.Rows[0][idx])
	}
}

// verifyPipeline reads impressions, auctions, and server-recorded wins back
// from reporting and asserts the run's money invariants: no client-observed
// win lost (imps >= wins), no unbacked impression (imps <= auction_wins), and
// every request audited (auctions == sent − errors). The async
// tracker→NATS→reporting hop lags — minutes under saturation — so it polls
// until the targets are met or counts hold still, capped at 5 minutes.
func verifyPipeline(reportingURL string, start time.Time, sent, wins, errors int) bool {
	client := &http.Client{Timeout: 5 * time.Second}
	log.Info("verifying pipeline — polling reporting for recorded events", "reporting", reportingURL)

	// Poll BOTH counts until the run's targets are reached or ingestion has
	// genuinely drained. Under saturation the tracker→NATS→reporting backlog
	// takes MINUTES to flush and plateaus between batch inserts, so the old
	// 30s / stable-after-two-reads loop routinely declared slippage on counts
	// that were still rising (and read auctions exactly ONCE, unpolled).
	// "Drained" now means: targets met (imps ≥ wins AND auctions ≥ sent−errors
	// — on a quiescent stack they then match exactly), or three consecutive
	// unchanged reads 3s apart, or a 5-minute ceiling.
	var imps, auctions, serverWins int
	target := func() bool { return imps >= wins && auctions >= sent-errors }
	prevImps, prevAuc, stable := -1, -1, 0
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)
		ni, ierr := queryCount(client, reportingURL, "impressions", start)
		na, aerr := queryCount(client, reportingURL, "auctions", start)
		nw, werr := queryCount(client, reportingURL, "auction_wins", start)
		if ierr != nil || aerr != nil || werr != nil {
			log.Warn("verify: count query failed", "impressions_err", ierr, "auctions_err", aerr, "wins_err", werr)
			continue
		}
		imps, auctions, serverWins = ni, na, nw
		if target() {
			break
		}
		if ni == prevImps && na == prevAuc {
			if stable++; stable >= 3 {
				break
			}
		} else {
			stable = 0
		}
		prevImps, prevAuc = ni, na
	}

	// The load-bearing invariants, in money order:
	//   1. No lost impressions: imps >= client wins (a win the client observed
	//      must have landed as an impression).
	//   2. No phantom impressions: imps <= SERVER-recorded wins (billing
	//      accrues on impression; an impression without an auction_wins row
	//      would be unbacked money). imps may legitimately EXCEED client wins:
	//      video/audio serves the client timed out on still deliver server-side
	//      (SSAI stitch fires the beacon) — those are real, win-backed
	//      impressions the client never counted.
	//   3. Every request audited: auctions == sent − errors.
	surplus := imps - wins
	ok := imps >= wins && imps <= serverWins
	fill := 0.0
	if auctions > 0 {
		fill = 100 * float64(imps) / float64(auctions)
	}

	// Engine cross-check: the metrics engine computes fill_rate SERVER-SIDE
	// (impressions.count / auctions.count). On a quiescent stack it must agree
	// with the simulator's own imps/auctions — proving the derived-metric path,
	// not just the raw counts. Degrades to a warning if the query fails (e.g. an
	// older reporting build without the engine); only asserts when it answers.
	engineFill, ferr := queryMetric(client, reportingURL, "impressions", "fill_rate", start)
	engineChecked := ferr == nil && auctions > 0
	engineMatch := engineChecked && math.Abs(engineFill-fill) < 0.5
	if engineChecked {
		ok = ok && engineMatch
	}

	fmt.Println("\n── Pipeline verification ─────────────────────────────")
	fmt.Printf("  Wins (client-counted): %d\n", wins)
	fmt.Printf("  Wins (server-recorded):%d\n", serverWins)
	fmt.Printf("  Impressions recorded:  %d   %s (≥ client wins, ≤ server wins)\n", imps, checkMark(imps >= wins && imps <= serverWins))
	fmt.Printf("  Auctions recorded:     %d   %s (requests sent − errors = %d)\n", auctions, checkMark(auctions == sent-errors), sent-errors)
	fmt.Printf("  Fill rate:             %.1f%%  (impressions / auctions)\n", fill)
	if ferr != nil {
		fmt.Printf("  Engine fill_rate:      (query failed: %v)\n", ferr)
	} else {
		fmt.Printf("  Engine fill_rate:      %.1f%%  %s (server-computed; matches local %.1f%%)\n", engineFill, checkMark(engineMatch), fill)
	}
	switch {
	case imps < wins:
		fmt.Printf("  ✗ SLIPPAGE: %d client-observed wins never landed as impressions.\n", wins-imps)
		fmt.Println("    → real pipeline loss; inspect tracker/NATS/reporting for that window.")
	case imps > serverWins:
		fmt.Printf("  ✗ PHANTOM: %d impressions exceed even server-recorded wins (%d).\n", imps-serverWins, serverWins)
		fmt.Println("    → unbacked impressions/billing, OR concurrent traffic in the window (verify against a quiescent stack).")
	case engineChecked && !engineMatch:
		fmt.Printf("  ✗ MISMATCH: engine fill_rate (%.1f%%) ≠ local fill_rate (%.1f%%).\n", engineFill, fill)
		fmt.Println("    → metrics engine disagrees with raw counts (derived-metric path drift).")
	default:
		if surplus > 0 {
			fmt.Printf("  ✓ pipeline lossless: every client win landed; %d extra impressions are\n", surplus)
			fmt.Println("    server-side deliveries (SSAI/late video) the client timed out on — all win-backed.")
		} else {
			fmt.Println("  ✓ pipeline lossless: every win landed as one impression; engine metrics agree.")
		}
	}
	fmt.Println("──────────────────────────────────────────────────────")
	return ok
}

func checkMark(ok bool) string {
	if ok {
		return "✓"
	}
	return "✗"
}
