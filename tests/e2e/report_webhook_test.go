//go:build e2e

// Report webhook delivery — the report-runner announces a finished job on
// adtech.report.completed; the webhooks dispatcher POSTs it to the owning
// account's subscriptions. End to end through the real stack: subscribe →
// submit a job with delivery=webhook → the in-cluster dispatcher calls back
// to a host-reachable test server with the completion envelope.
package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestReportWebhookDelivery(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "report-webhook")
	uniq := fmt.Sprintf("rwh-%d", time.Now().UnixNano())

	// Host-reachable sink for the dispatcher's POSTs.
	var mu sync.Mutex
	var bodies []string
	ts := harness.HostReachableServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		rw.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	email := "rwh-" + w.AdvAcc.ID + "@e2e.local"
	h.CreateLoginUser(t, w.AdvAcc.ID, email, "e2e-pass", "owner")
	client := h.LoginAs(t, email, "e2e-pass")

	// Subscribe the account to report.completed.
	sub := h.APIJSON(t, client, http.MethodPost, "/v1/api/webhooks", fmt.Sprintf(`{
		"url": %q, "events": ["report.completed"]
	}`, ts.URL+"/hook"))
	if sub["id"] == nil {
		t.Fatalf("subscription create failed: %v", sub)
	}

	// Submit a job with delivery=webhook (no email recipient involved).
	res := h.APIJSON(t, client, http.MethodPost, "/v1/api/reports/jobs", fmt.Sprintf(`{
		"name": "webhook delivery %s",
		"query_config": {"table": "impressions", "metrics": ["count"]},
		"format": "csv",
		"delivery": "webhook"
	}`, uniq))
	jobID, _ := res["id"].(string)
	if jobID == "" {
		t.Fatalf("submit returned no job id: %v", res)
	}
	waitJobDone(t, h, client, jobID)

	// The dispatcher's POST arrives async (runner tick + dispatch retries).
	deadline := time.Now().Add(60 * time.Second)
	for {
		mu.Lock()
		joined := strings.Join(bodies, "\n")
		mu.Unlock()
		if strings.Contains(joined, jobID) {
			// Envelope carries the event type + the ReportCompletedEvent data.
			if !strings.Contains(joined, "report.completed") {
				t.Errorf("webhook body missing event type: %s", joined)
			}
			var probe struct {
				Data struct {
					DownloadURL string `json:"download_url"`
					AccountID   string `json:"account_id"`
				} `json:"data"`
			}
			for _, b := range strings.Split(joined, "\n") {
				if strings.Contains(b, jobID) {
					_ = json.Unmarshal([]byte(b), &probe)
				}
			}
			if probe.Data.DownloadURL == "" || !strings.Contains(probe.Data.DownloadURL, jobID) {
				t.Errorf("webhook payload missing download_url for job: %s", joined)
			}
			if probe.Data.AccountID != w.AdvAcc.ID {
				t.Errorf("webhook payload account = %s, want %s", probe.Data.AccountID, w.AdvAcc.ID)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no webhook delivery for job %s (got %d posts: %s)", jobID, len(bodies), joined)
		}
		time.Sleep(2 * time.Second)
	}
}
