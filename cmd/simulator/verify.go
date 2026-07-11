package main

import (
	"bytes"
	"encoding/json"
	"fmt"
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

// verifyPipeline reads impressions + auctions back from reporting and asserts
// they match what this run fired. The async tracker→NATS→reporting hop means
// the count lags, so it polls until stable (unchanged across two reads) or a
// timeout. Returns true when the core invariant (impressions == wins) holds.
func verifyPipeline(reportingURL string, start time.Time, sent, wins, errors int) bool {
	client := &http.Client{Timeout: 5 * time.Second}
	log.Info("verifying pipeline — polling reporting for recorded events", "reporting", reportingURL)

	// Poll impressions until the count stops changing (pipeline drained) or 30s.
	var imps, auctions, prev int
	stable := 0
	for i := 0; i < 20; i++ {
		time.Sleep(1500 * time.Millisecond)
		n, err := queryCount(client, reportingURL, "impressions", start)
		if err != nil {
			log.Warn("verify: impressions query failed", "error", err)
			continue
		}
		if n == prev {
			stable++
			if stable >= 2 {
				imps = n
				break
			}
		} else {
			stable = 0
		}
		prev = n
		imps = n
	}
	auctions, _ = queryCount(client, reportingURL, "auctions", start)

	// Report. The load-bearing invariant is impressions == wins (every win the
	// exchange handed back must have recorded exactly one impression).
	ok := imps == wins
	fill := 0.0
	if auctions > 0 {
		fill = 100 * float64(imps) / float64(auctions)
	}
	fmt.Println("\n── Pipeline verification ─────────────────────────────")
	fmt.Printf("  Wins fired:            %d\n", wins)
	fmt.Printf("  Impressions recorded:  %d   %s\n", imps, checkMark(imps == wins))
	fmt.Printf("  Auctions recorded:     %d   %s (requests sent − errors = %d)\n", auctions, checkMark(auctions == sent-errors), sent-errors)
	fmt.Printf("  Fill rate:             %.1f%%  (impressions / auctions)\n", fill)
	if !ok {
		fmt.Printf("  ✗ MISMATCH: recorded impressions (%d) ≠ wins fired (%d).\n", imps, wins)
		fmt.Println("    → pipeline slippage, OR concurrent traffic in the window (verify against a quiescent stack).")
	} else {
		fmt.Println("  ✓ pipeline lossless: every win landed as exactly one impression.")
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
