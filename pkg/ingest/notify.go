package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/email"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/ingestjobs"
)

// NotifyResult emails the outcome of a terminal ingest job to its NotifyEmails
// recipients (ADR 0008 Feature 3). It is a best-effort no-op when there are no
// recipients or no sender: an email is a courtesy, never a gate. A send error is
// logged (WARN) and swallowed so notification failure can't fail the ingest.
//
// jobErr == nil is a success (members added + match rate); a non-nil jobErr is a
// failure (the reject reason). One message per recipient so a bad address for
// one recipient doesn't drop the rest.
func NotifyResult(ctx context.Context, sender email.Sender, from string,
	job ingestjobs.Job, res ingestjobs.IngestResult, jobErr error, log *slog.Logger,
) {
	if sender == nil || len(job.NotifyEmails) == 0 {
		return
	}
	name := job.SegmentSpec.Name
	if name == "" {
		name = "audience"
	}

	var subject, body string
	if jobErr == nil {
		subject = fmt.Sprintf("Audience upload %q succeeded", name)
		body = fmt.Sprintf(
			"Your audience upload %q finished successfully.\n\n"+
				"Segment ID:    %s\nMembers added: %d\nValid rows:    %d\nMatched rows:  %d\nMatch rate:    %.1f%%\n",
			name, res.SegmentID, res.MembersAdded, res.ValidRows, res.MatchedRows, res.MatchRate*100)
	} else {
		subject = fmt.Sprintf("Audience upload %q failed", name)
		body = fmt.Sprintf(
			"Your audience upload %q could not be processed.\n\nReason: %s\n",
			name, jobErr.Error())
	}

	for _, to := range job.NotifyEmails {
		to = strings.TrimSpace(to)
		if to == "" {
			continue
		}
		if err := sender.Send(ctx, email.Message{To: to, From: from, Subject: subject, Body: body}); err != nil {
			log.Warn("ingest: notify email send failed", "to", to, "job", job.ID, "error", err)
		}
	}
}

// SanitizeNotifyEmails cleans a recipient list: trims, drops empties, requires a
// basic "@" sanity check, and de-duplicates (first occurrence wins, order
// preserved). Shared by the gateway upload (uploader + additional_emails) and
// any other producer building a job's NotifyEmails.
func SanitizeNotifyEmails(emails []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(emails))
	for _, e := range emails {
		e = strings.TrimSpace(e)
		if e == "" || !strings.Contains(e, "@") {
			continue
		}
		if _, dup := seen[e]; dup {
			continue
		}
		seen[e] = struct{}{}
		out = append(out, e)
	}
	return out
}
