//go:build e2e

// Scheduled-report path of the async report builder, driven end-to-end
// against the LIVE in-cluster worker: a saved report with an interval
// schedule is picked up by report-runner's scheduler tick, enqueued as a
// report job, executed (query → artifact in Minio), and the download link is
// emailed via Mailpit. Nothing here runs in-process — this asserts the
// production wiring (worker pod + SMTP + object store + gateway download).
package e2e

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestScheduledReportBecomesJobAndEmails(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)

	uniq := fmt.Sprintf("rr-%d", time.Now().UnixNano())
	ownerEmail := uniq + "-adv@api.test"
	adv := h.Signup(t, "RR Adv", ownerEmail, "pw-e2e-1", "advertiser")

	// A daily, email-delivered saved report. The live worker's scheduler tick
	// (report_runner.schedule_interval, default 60s) enqueues it because it
	// has never run.
	h.APIJSON(t, adv, http.MethodPost, "/v1/api/reports/saved", `{
		"name":"Daily Impressions","query_config":{"table":"impressions","metrics":["count"]},
		"schedule":"@daily","delivery":"email","format":"csv"
	}`)

	// The schedule surfaces as a done job in the account's job list.
	var jobID string
	harness.WaitFor(t, 2*time.Minute, "scheduled report enqueued + executed by the live worker", func() bool {
		res := h.APIJSON(t, adv, http.MethodGet, "/v1/api/reports/jobs", "")
		items, _ := res["items"].([]any)
		for _, it := range items {
			j, _ := it.(map[string]any)
			if j["source"] == "schedule" && j["status"] == "done" {
				jobID, _ = j["id"].(string)
				return true
			}
			if j["source"] == "schedule" && j["status"] == "failed" {
				t.Fatalf("scheduled job failed: %v", j["error"])
			}
		}
		return false
	})

	// The report-ready email reached the owner with a working download link.
	msg := h.WaitForMail(t, 30*time.Second, ownerEmail)
	if !strings.Contains(msg.Subject, "Daily Impressions") {
		t.Errorf("email subject %q missing report name", msg.Subject)
	}
	body := h.MailpitBody(t, msg.ID)
	wantLink := "/v1/api/reports/jobs/" + jobID + "/download"
	if !strings.Contains(body, wantLink) {
		t.Errorf("email body missing download link %s:\n%s", wantLink, body)
	}

	// The link works for the owner's session (auth required — the bucket is
	// private, so this gateway stream is the only way in).
	status := h.APIStatus(t, adv, http.MethodGet, wantLink, "")
	if status != http.StatusOK {
		t.Errorf("download via emailed link: status %d, want 200", status)
	}

	// Idempotency: the @daily schedule was marked run at enqueue, so exactly
	// one schedule-sourced job exists for it.
	res := h.APIJSON(t, adv, http.MethodGet, "/v1/api/reports/jobs", "")
	items, _ := res["items"].([]any)
	scheduleJobs := 0
	for _, it := range items {
		j, _ := it.(map[string]any)
		if j["source"] == "schedule" {
			scheduleJobs++
		}
	}
	if scheduleJobs != 1 {
		t.Errorf("schedule-sourced jobs = %d, want exactly 1 (last_run_at not honoured?)", scheduleJobs)
	}
}
