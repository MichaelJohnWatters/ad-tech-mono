package main

// ingest_worker.go — the audience ingest worker (ADR 0007).
//
// Mirrors cmd/report-runner's executor loop: on a ticker it reclaims lapsed
// leases, then drains the audience_ingest_jobs queue via ClaimOne until empty.
// For each claimed job it runs the shared processStagedFile under a lease
// heartbeat, then MarkDone (with the result counts) or MarkFailed. It is
// multi-replica safe — ClaimOne is FOR UPDATE SKIP LOCKED and the lease means a
// crashed worker's job is reclaimed by a peer, not double-run by a booting one.
//
// The drop-zone poller (onboarding.go tick) is the producer; this is the
// consumer. Keeping them in the same process reuses all the onboarding wiring
// (object store, Postgres, NATS, pipeline, audience store).

import (
	"context"
	"log/slog"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/ingest"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/ingestjobs"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
)

// startIngestWorker launches the queue-draining loop over the same onboarder o
// (which already holds the job store + processor deps).
func startIngestWorker(cfg *config.Config, o *onboarder, log *slog.Logger, lc *lifecycle.Lifecycle) {
	if o.jobs == nil {
		return
	}
	interval := keys.Pipeline.IngestWorkerInterval.Get(cfg)

	// Crash recovery at boot: reclaim jobs whose lease lapsed (claimant dead).
	if n, err := o.jobs.ReclaimExpired(context.Background()); err != nil {
		log.Warn("ingest worker: reclaim expired at boot", "error", err)
	} else if n > 0 {
		log.Info("ingest worker: reclaimed expired jobs at boot", "count", n)
	}

	loopCtx, stop := context.WithCancel(context.Background())
	lc.OnShutdown("ingest-worker", func(_ context.Context) error { stop(); return nil })
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		o.drainIngest(loopCtx)
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-t.C:
				o.drainIngest(loopCtx)
			}
		}
	}()
	log.Info("audience ingest worker running", "interval", interval.String())
}

// drainIngest reclaims lapsed leases, then claims + processes jobs until the
// queue is empty (or the context is cancelled).
func (o *onboarder) drainIngest(ctx context.Context) {
	if n, err := o.jobs.ReclaimExpired(ctx); err == nil && n > 0 {
		o.log.Info("ingest worker: reclaimed expired jobs", "count", n)
	}
	for {
		if ctx.Err() != nil {
			return
		}
		job, err := o.jobs.ClaimOne(ctx)
		if err != nil {
			o.log.Error("ingest worker: claim failed", "error", err)
			return
		}
		if job == nil {
			return // queue drained
		}
		o.runIngestJob(ctx, *job)
	}
}

// runIngestJob processes one claimed job under a lease heartbeat, then records
// the terminal state. An infraErr (retryable) is left for the lease to lapse
// and a peer to reclaim — UNLESS the job has exhausted its attempts, in which
// case it is marked failed so it stops cycling. A content failure returns nil
// error with a terminal result and is marked done.
func (o *onboarder) runIngestJob(ctx context.Context, job ingestjobs.Job) {
	// trace_id: the job's ing_ lineage id — the same string on the resulting
	// profile_signals rows greps these lines.
	log := o.log.With("trace_id", job.Trace(), "job", job.ID)
	// Heartbeat: extend the lease while processing so a long file doesn't get
	// reclaimed out from under us.
	hbCtx, stopHB := context.WithCancel(ctx)
	defer stopHB()
	go func() {
		t := time.NewTicker(ingestjobs.LeaseTTL / 3)
		defer t.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-t.C:
				if err := o.jobs.ExtendLease(ctx, job.ID); err != nil {
					log.Warn("ingest worker: extend lease failed", "error", err)
				}
			}
		}
	}()

	result, err := o.processStagedFile(ctx, job)
	stopHB()
	if err != nil {
		if ingest.IsInfra(err) && job.Attempts < job.MaxAttempts {
			// Retryable + attempts remain: leave it. The lease lapses and a
			// worker reclaims it; nothing is recorded (the run never happened).
			log.Warn("ingest worker: job left for retry",
				"attempts", job.Attempts, "max", job.MaxAttempts, "error", err)
			return
		}
		if markErr := o.jobs.MarkFailed(ctx, job.ID, err.Error()); markErr != nil {
			log.Error("ingest worker: mark failed", "error", markErr)
		}
		// Best-effort completion email (ADR 0008 Feature 3).
		ingest.NotifyResult(ctx, o.emailSender, o.emailFrom, job, result, err, o.log)
		return
	}
	if err := o.jobs.MarkDone(ctx, job.ID, result); err != nil {
		log.Error("ingest worker: mark done", "error", err)
	}
	// Best-effort completion email (ADR 0008 Feature 3).
	ingest.NotifyResult(ctx, o.emailSender, o.emailFrom, job, result, nil, o.log)
}
