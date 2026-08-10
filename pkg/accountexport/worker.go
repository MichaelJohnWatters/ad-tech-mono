package accountexport

import (
	"context"
	"log/slog"
)

// Worker drains the account-export queue: claim → build the archive → mark done
// (or failed). Runs inside the report-runner's tick loop, which already owns the
// object store the Builder writes to.
type Worker struct {
	Store   Store
	Builder *Builder
	Log     *slog.Logger
}

// RunOnce claims and processes at most one queued job. Returns claimed=false
// when the queue is empty; callers loop until then. A build failure is recorded
// on the job (status=failed) and returns claimed=true, err=nil — a bad export
// must not wedge the drain loop.
func (w *Worker) RunOnce(ctx context.Context) (bool, error) {
	job, err := w.Store.ClaimNext(ctx)
	if err != nil {
		return false, err
	}
	if job == nil {
		return false, nil
	}
	key, size, err := w.Builder.Build(ctx, job.AccountID, job.ID)
	if err != nil {
		w.Log.Error("account export build failed", "job", job.ID, "account", job.AccountID, "error", err)
		if merr := w.Store.MarkFailed(ctx, job.ID, err.Error()); merr != nil {
			w.Log.Error("account export mark-failed failed", "job", job.ID, "error", merr)
		}
		return true, nil
	}
	if err := w.Store.MarkDone(ctx, job.ID, w.Builder.Bucket, key, size); err != nil {
		w.Log.Error("account export mark-done failed", "job", job.ID, "error", err)
		return true, nil
	}
	w.Log.Info("account export completed", "job", job.ID, "account", job.AccountID, "bytes", size, "key", key)
	return true, nil
}
