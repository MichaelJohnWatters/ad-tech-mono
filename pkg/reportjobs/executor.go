package reportjobs

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/email"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/reportrunner"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
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

	// Events publishes report.completed after the artifact is durable —
	// the webhooks dispatcher turns it into subscription deliveries
	// (delivery=webhook, and any account that subscribed regardless of the
	// job's own delivery mode). Nil = no announcements.
	Events *events.Publisher

	// SegmentMembers resolves jobs whose QueryConfig.Table is
	// TableSegmentMembers — the audience segment EXPORT path (profile
	// store payoff valve). Rides the same queue/artifact/status/download
	// machinery as analytics reports; only the row source differs (Postgres
	// memberships instead of the reporting query API). Nil = unsupported.
	SegmentMembers SegmentMembersFunc
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

	// Heartbeat the lease while working: a live worker's job is never
	// reclaimable, however long the query/render takes; a dead worker's
	// lease lapses within LeaseTTL and a peer picks the job up.
	hbCtx, stopHB := context.WithCancel(ctx)
	defer stopHB()
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-t.C:
				if err := e.Store.ExtendLease(hbCtx, j.ID); err != nil {
					log.Warn("lease heartbeat failed", "error", err)
				}
			}
		}
	}()

	qctx := ctx
	if e.QueryTimeout > 0 {
		var cancel context.CancelFunc
		qctx, cancel = context.WithTimeout(ctx, e.QueryTimeout)
		defer cancel()
	}
	var res analytics.QueryResult
	if j.QueryConfig.Table == TableSegmentMembers {
		if e.SegmentMembers == nil {
			e.fail(ctx, log, j.ID, fmt.Errorf("segment export not supported by this worker"))
			return true, nil
		}
		res, err = e.SegmentMembers(qctx, j.AccountID, j.QueryConfig.Filters["segment_id"])
	} else {
		res, err = e.Query(qctx, j.AccountID, j.QueryConfig)
	}
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
	// Announce completion regardless of delivery mode — the dispatcher only
	// delivers to accounts holding a report.completed subscription, so this
	// is subscription-gated fan-out, not a broadcast.
	if e.Events != nil {
		ev := events.ReportCompletedEvent{
			SchemaVersion: events.CurrentSchemaVersion,
			AccountID:     j.AccountID,
			JobID:         j.ID,
			Name:          j.Name,
			Format:        j.Format,
			RowCount:      art.Rows,
			ArtifactBytes: art.Bytes,
			DownloadURL:   e.GatewayURL + routes.APIReportJobs + "/" + j.ID + "/download",
			ExpiresAt:     j.ExpiresAt,
			Timestamp:     e.now(),
		}
		if err := e.Events.PublishJSON(ctx, events.SubjectReportCompleted, ev); err != nil {
			log.Error("report job completion publish failed", "error", err)
		}
	}
	return true, nil
}

func (e *Executor) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now().UTC()
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
