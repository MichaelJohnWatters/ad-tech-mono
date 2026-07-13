package reportjobs

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/email"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/reportrunner"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects"
)

// Executor drains the job queue: claim → query reporting → render the
// artifact → upload → mark done → deliver. One job at a time; the worker
// loops RunOnce until the queue is empty each tick.
type Executor struct {
	Store        JobStore
	Query        reportrunner.QueryFunc // reporting HTTP query (or a fake)
	Objects      objects.Store
	Bucket       string
	Email        email.Sender
	From         string
	GatewayURL   string        // public gateway base URL for download links
	QueryTimeout time.Duration // per-job bound on the reporting query
	Now          func() time.Time
	Log          *slog.Logger
}

// RunOnce claims and fully processes at most one job. It returns whether a
// job was claimed; job-level failures are recorded on the job (MarkFailed)
// and do NOT return an error — only store-level problems do.
func (e *Executor) RunOnce(ctx context.Context) (bool, error) {
	j, err := e.Store.ClaimOne(ctx)
	if err != nil {
		return false, fmt.Errorf("claim: %w", err)
	}
	if j == nil {
		return false, nil
	}
	log := e.Log.With("job", j.ID, "account_id", j.AccountID, "name", j.Name, "format", j.Format)

	qctx := ctx
	if e.QueryTimeout > 0 {
		var cancel context.CancelFunc
		qctx, cancel = context.WithTimeout(ctx, e.QueryTimeout)
		defer cancel()
	}
	res, err := e.Query(qctx, j.AccountID, j.QueryConfig)
	if err != nil {
		e.fail(ctx, log, j.ID, fmt.Errorf("query: %w", err))
		return true, nil
	}

	data, fw, err := renderArtifact(j.Format, res)
	if err != nil {
		e.fail(ctx, log, j.ID, err)
		return true, nil
	}

	key := j.AccountID + "/" + j.ID + "." + fw.Ext()
	if err := e.Objects.Put(ctx, e.Bucket, key, bytes.NewReader(data), int64(len(data)), fw.ContentType()); err != nil {
		e.fail(ctx, log, j.ID, fmt.Errorf("store artifact: %w", err))
		return true, nil
	}

	art := Artifact{Bucket: e.Bucket, Key: key, Bytes: int64(len(data)), Rows: int64(len(res.Rows))}
	if err := e.Store.MarkDone(ctx, j.ID, art); err != nil {
		return true, fmt.Errorf("mark done %s: %w", j.ID, err)
	}
	log.Info("report job done", "rows", art.Rows, "bytes", art.Bytes)

	// Delivery is best-effort AFTER the artifact is durable: a bounced email
	// must not fail a job whose result is downloadable.
	if j.Delivery == DeliveryEmail && j.Recipient != "" {
		if err := e.Email.Send(ctx, e.linkEmail(j, art)); err != nil {
			log.Error("report job email delivery failed", "recipient", j.Recipient, "error", err)
		}
	}
	return true, nil
}

func (e *Executor) fail(ctx context.Context, log *slog.Logger, id string, cause error) {
	log.Error("report job failed", "error", cause)
	if err := e.Store.MarkFailed(ctx, id, cause.Error()); err != nil {
		log.Error("report job mark-failed failed", "error", err)
	}
}

func (e *Executor) linkEmail(j *Job, art Artifact) email.Message {
	link := e.GatewayURL + routes.APIReportJobs + "/" + j.ID + "/download"
	return email.Message{
		To:      j.Recipient,
		From:    e.From,
		Subject: "Report ready: " + j.Name,
		Body: fmt.Sprintf("Your report %q is ready (%d rows, %s).\n\nDownload (sign-in required): %s\n\nThe download link expires %s.\n",
			j.Name, art.Rows, j.Format, link, j.ExpiresAt.UTC().Format(time.RFC1123)),
	}
}
