//go:build e2e

// Scheduled-report runner: a saved report with an interval schedule + email
// delivery is run on its cadence and emailed to the account owner. This drives
// the REAL Postgres store (saved_reports join team_members) + the REAL reporting
// query against the live cluster, with an in-memory email sender to assert
// delivery. Idempotency: a second run doesn't re-deliver (last_run_at honoured).
package e2e

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/email"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/reportrunner"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestReportRunnerViaAPI(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.Reset(t)

	uniq := fmt.Sprintf("rr-%d", time.Now().UnixNano())
	ownerEmail := uniq + "-adv@api.test"
	adv := h.Signup(t, "RR Adv", ownerEmail, "pw-e2e-1", "advertiser")

	// A daily, email-delivered saved report.
	h.APIJSON(t, adv, http.MethodPost, "/v1/api/reports/saved", `{
		"name":"Daily Impressions","query_config":{"table":"impressions","metrics":["count"]},
		"schedule":"@daily","delivery":"email"
	}`)

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	mem := email.NewMemory(quiet)
	runner := &reportrunner.Runner{
		Store: reportrunner.NewPostgresStore(h.DB),
		Query: reportrunner.HTTPQuery(h.URLs.Reporting, &http.Client{Timeout: 30 * time.Second}),
		Email: mem,
		From:  "reports@adtech.test",
		Now:   time.Now,
		Log:   quiet,
	}

	// First run: the never-run @daily report is due → one email to the owner.
	ran, err := runner.RunDue(context.Background())
	if err != nil {
		t.Fatalf("RunDue: %v", err)
	}
	if ran < 1 {
		t.Fatalf("reports_run = %d, want >= 1", ran)
	}
	var delivered *email.Message
	for i, m := range mem.Sent() {
		if m.To == ownerEmail && strings.Contains(m.Subject, "Daily Impressions") {
			delivered = &mem.Sent()[i]
		}
	}
	if delivered == nil {
		t.Fatalf("no report email delivered to %s; sent=%d", ownerEmail, len(mem.Sent()))
	}
	if !strings.Contains(delivered.Body, "Scheduled report: Daily Impressions") {
		t.Errorf("email body missing report header: %q", delivered.Body)
	}

	// Second run: idempotent — last_run_at now set, so it's not due again.
	mem2 := email.NewMemory(quiet)
	runner.Email = mem2
	if _, err := runner.RunDue(context.Background()); err != nil {
		t.Fatalf("second RunDue: %v", err)
	}
	for _, m := range mem2.Sent() {
		if m.To == ownerEmail {
			t.Errorf("report re-delivered on the second run — last_run_at not honoured")
		}
	}
}
