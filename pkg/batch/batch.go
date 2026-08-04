// Package batch is the conductor for the platform's data chain: an explicit,
// completion-ordered sequence of batch steps, replacing the old
// stagger-by-cron-offset coordination (which encoded real dependencies in
// schedule minutes nobody maintains, and failed silently when a step overran
// its slot).
//
// The DAG is code: a []Step run strictly in order, each step starting only
// when the previous one has actually finished. A CRITICAL step's failure
// aborts the chain (everything after it is recorded as skipped); a
// non-critical failure is recorded and the chain continues — the platform's
// batch jobs are idempotent wholesale-recomputers, so "continue on stale
// input" is safe where marked. Every step writes a batch_runs row, giving
// per-run observability the offset lattice never had.
//
// When the chain outgrows a straight line (parallel branches, multi-day
// backfills, non-replayable steps), lift these same Step values into a real
// workflow engine (Temporal fits the Go ethos) — the interface is the
// migration seam.
package batch

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

// Step is one link in the chain. Run returns a short human-readable detail
// string (row counts, files compacted, …) recorded on the batch_runs row.
type Step struct {
	Name string
	// Critical: a failure means downstream steps can't produce meaningful
	// results — abort the chain (remaining steps recorded as skipped).
	// Non-critical failures are recorded and the chain continues.
	Critical bool
	Run      func(ctx context.Context) (detail string, err error)
}

// StepOutcome is one step's recorded result.
type StepOutcome struct {
	Step     string
	Status   string // done | failed | skipped
	Detail   string
	Err      error
	Duration time.Duration
}

// ChainResult is the whole run's outcome.
type ChainResult struct {
	RunID    string
	Steps    []StepOutcome
	Aborted  bool // a critical step failed
	Failed   int
	Duration time.Duration
}

// RunChain executes steps in order, recording each to the recorder (nil =
// no recording, e.g. unit tests). Returns an error only when a CRITICAL
// step failed — matching CronJob semantics, where a non-zero exit means
// "this run needs attention", not "some optional step hiccuped".
func RunChain(ctx context.Context, steps []Step, rec *Recorder, log *slog.Logger) (ChainResult, error) {
	if log == nil {
		log = slog.Default()
	}
	res := ChainResult{RunID: uuid.NewString()}
	// Steps read the run id via RunIDFromContext to stamp lineage (e.g. the
	// profile-builder's membership origin_trace) with the batch_runs.run_id.
	ctx = context.WithValue(ctx, runIDKey{}, res.RunID)
	start := time.Now()
	aborted := false

	for i, s := range steps {
		if aborted {
			rec.record(ctx, res.RunID, i, s, "skipped", "", nil, time.Now(), nil)
			res.Steps = append(res.Steps, StepOutcome{Step: s.Name, Status: "skipped"})
			continue
		}
		stepStart := time.Now()
		rowID := rec.record(ctx, res.RunID, i, s, "running", "", nil, stepStart, nil)
		detail, err := s.Run(ctx)
		finished := time.Now()
		outcome := StepOutcome{Step: s.Name, Detail: detail, Err: err, Duration: finished.Sub(stepStart)}
		if err != nil {
			outcome.Status = "failed"
			res.Failed++
			if s.Critical {
				aborted = true
				log.Error("batch step failed (CRITICAL — aborting chain)", "step", s.Name, "error", err)
			} else {
				log.Error("batch step failed (non-critical — continuing)", "step", s.Name, "error", err)
			}
		} else {
			outcome.Status = "done"
			log.Info("batch step done", "step", s.Name, "detail", detail, "duration_ms", outcome.Duration.Milliseconds())
		}
		rec.update(ctx, rowID, outcome.Status, detail, err, finished)
		res.Steps = append(res.Steps, outcome)
	}

	res.Aborted = aborted
	res.Duration = time.Since(start)
	if aborted {
		return res, fmt.Errorf("batch chain aborted: a critical step failed")
	}
	return res, nil
}

// runIDKey carries the chain's run id in the step context.
type runIDKey struct{}

// RunIDFromContext returns the batch_runs.run_id RunChain injected into the
// step context, or "" outside a chain run.
func RunIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(runIDKey{}).(string)
	return id
}

// Recorder persists step outcomes to batch_runs. Nil-safe: a nil recorder
// (or one whose DB is down) degrades to log-only — the chain itself must
// not depend on its own telemetry.
type Recorder struct {
	DB  *sql.DB
	Log *slog.Logger
}

func (r *Recorder) record(ctx context.Context, runID string, seq int, s Step, status, detail string, err error, started time.Time, finished *time.Time) string {
	if r == nil || r.DB == nil {
		return ""
	}
	var id string
	errMsg := ""
	if err != nil {
		errMsg = err.Error()
	}
	qerr := r.DB.QueryRowContext(ctx, `
INSERT INTO batch_runs (run_id, seq, step, status, critical, detail, error, started_at, finished_at)
VALUES ($1, $2, $3, $4, $5, NULLIF($6,''), NULLIF($7,''), $8, $9)
RETURNING id::text`,
		runID, seq, s.Name, status, s.Critical, detail, errMsg, started, finished).Scan(&id)
	if qerr != nil && r.Log != nil {
		r.Log.Warn("batch recorder insert failed", "step", s.Name, "error", qerr)
	}
	return id
}

func (r *Recorder) update(ctx context.Context, rowID, status, detail string, err error, finished time.Time) {
	if r == nil || r.DB == nil || rowID == "" {
		return
	}
	errMsg := ""
	if err != nil {
		errMsg = err.Error()
	}
	if _, qerr := r.DB.ExecContext(ctx, `
UPDATE batch_runs SET status = $2, detail = NULLIF($3,''), error = NULLIF($4,''), finished_at = $5
WHERE id = $1::uuid`, rowID, status, detail, errMsg, finished); qerr != nil && r.Log != nil {
		r.Log.Warn("batch recorder update failed", "row", rowID, "error", qerr)
	}
}
