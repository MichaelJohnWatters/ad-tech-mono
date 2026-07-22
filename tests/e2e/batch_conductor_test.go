//go:build e2e

// Batch-conductor gate — the data chain runs completion-ordered against the
// live stack:
//
//   - every step green (checkpoint → rollups → ch-parquet-export →
//     profile-builder → privacy delete → verify), recorded in batch_runs;
//   - ORDERING proven from the recorded timestamps: step N starts only
//     after step N-1 finished — the property cron offsets only approximated;
//   - a critical-step failure (pipeline down) aborts the chain and records
//     the rest as skipped, instead of running against a broken foundation.
package e2e

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/batch"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestBatchConductorChain(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "batch-chain")
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Give the chain something real to chew on: an identity edge (clusters),
	// a membership (expansion), and a pending level-3 deletion (privacy).
	uniq := int(time.Now().UnixNano())
	user := "chain-user-" + itoa(uniq)
	h.AddIdentityEdge(t, user, "chain-dev-"+itoa(uniq), "cross_device")
	seg := h.CreateSegment(t, w.AdvAcc, "chain-seg-"+itoa(uniq))
	h.AddUserToSegment(t, seg, user)
	delUser := "chain-del-" + itoa(uniq)
	h.AddIdentityEdge(t, delUser, "chain-del-dev-"+itoa(uniq), "cross_device")
	h.SetOptOut(t, delUser, 3)

	deps := batch.Deps{
		DB:           h.DB,
		Lake:         lakeStore(t, h),
		PipelineURL:  routes.DefaultPipelineURL,
		ReportingURL: routes.DefaultReportingURL,
		HTTP:         &http.Client{Timeout: 2 * time.Minute},
		Log:          quiet,
		// Postgres-only privacy extras here: this test asserts chain ordering +
		// the PG identity-edge purge. ClickHouse signal-purge depth (SignalsPurger)
		// is covered by privacy_test. The lake LakePurger was retired (ADR 0006).
	}
	rec := &batch.Recorder{DB: h.DB, Log: quiet}
	res, err := batch.RunChain(context.Background(), batch.StandardChain(deps), rec, quiet)
	if err != nil {
		t.Fatalf("chain failed: %v (steps: %+v)", err, res.Steps)
	}
	if res.Failed != 0 {
		t.Fatalf("chain had %d failed steps: %+v", res.Failed, res.Steps)
	}

	// The recorded rows prove completion-ordering: every step's started_at
	// is >= the previous step's finished_at.
	rows, err := h.DB.Query(`
SELECT step, status, started_at, finished_at
FROM batch_runs WHERE run_id = $1::uuid ORDER BY seq`, res.RunID)
	if err != nil {
		t.Fatalf("read batch_runs: %v", err)
	}
	defer rows.Close()
	var prevFinished time.Time
	var steps []string
	for rows.Next() {
		var step, status string
		var started, finished time.Time
		if err := rows.Scan(&step, &status, &started, &finished); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if status != "done" {
			t.Errorf("step %s recorded as %s, want done", step, status)
		}
		if !prevFinished.IsZero() && started.Before(prevFinished) {
			t.Errorf("step %s started (%s) before previous step finished (%s) — chain not completion-ordered",
				step, started, prevFinished)
		}
		prevFinished = finished
		steps = append(steps, step)
	}
	want := []string{"checkpoint", "rollup:minute", "rollup:hourly",
		"rollup:daily", "rollup:monthly", "ch-parquet-export", "profile-builder", "privacy-delete", "privacy-verify"}
	if strings.Join(steps, ",") != strings.Join(want, ",") {
		t.Errorf("recorded steps = %v, want %v", steps, want)
	}

	// The chain did real work: the level-3 user is purged and the cluster
	// materialized.
	if got := h.IdentityEdgeCount(t, delUser); got != 0 {
		t.Errorf("privacy step didn't purge %s (%d edges remain)", delUser, got)
	}
	var clusterMembers int
	_ = h.DB.QueryRow(`SELECT count(*) FROM identity_clusters`).Scan(&clusterMembers)
	if clusterMembers == 0 {
		t.Error("profile-builder step didn't materialize identity_clusters")
	}

	// Abort semantics: a dead pipeline fails the CRITICAL checkpoint; the
	// rest of the chain is recorded as skipped, never run.
	badDeps := deps
	badDeps.PipelineURL = "http://localhost:1" // nothing listens
	badDeps.HTTP = &http.Client{Timeout: 3 * time.Second}
	badRes, badErr := batch.RunChain(context.Background(), batch.StandardChain(badDeps), rec, quiet)
	if badErr == nil {
		t.Fatal("chain with dead pipeline should fail (critical checkpoint)")
	}
	var skipped int
	if err := h.DB.QueryRow(`SELECT count(*) FROM batch_runs WHERE run_id = $1::uuid AND status = 'skipped'`,
		badRes.RunID).Scan(&skipped); err != nil {
		t.Fatalf("count skipped: %v", err)
	}
	if skipped != len(want)-1 {
		t.Errorf("skipped steps = %d, want %d (everything after checkpoint)", skipped, len(want)-1)
	}
}

