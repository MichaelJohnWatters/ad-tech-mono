//go:build e2e

// The staff "Onboarding & Expansion" demo's UPLOAD step routes through the REAL
// ingest path (stage → enqueue → claim → process), the same code a customer
// upload runs — so its profile_signal carries a proper ing_-format
// ingest_trace_id and never an ing_-format trace_id, exactly like a real upload.
// (Previously the demo hand-wrote the row and stamped a request trace_id with no
// ingest_trace_id.)
package e2e

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/idgen"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestDemoOnboardingUploadCarriesIngestTraceID(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)

	// staff:owner ⇒ support:update, required to run the demo. Unique email so the
	// completion-email assertion below can't collide with a prior run's inbox.
	staff := h.CreateStaff(t, "demo-ingtrace-staff")
	email := fmt.Sprintf("demo-ingtrace-%d@e2e.local", time.Now().UnixNano())
	h.CreateLoginUser(t, staff.ID, email, "e2e-pass", "owner")
	client := h.LoginAs(t, email, "e2e-pass")

	// Scope every assertion to rows observed from now on — ClickHouse is
	// append-only and the demo's resetDemo only wipes Postgres, so prior demo
	// runs leave stale profile_signals for the same synthetic account.
	startTS := time.Now().UTC().Add(-5 * time.Second).Format("2006-01-02 15:04:05")

	req, err := http.NewRequest(http.MethodPost, h.URLs.Gateway+routes.APIDemoOnboardingRun, nil)
	if err != nil {
		t.Fatalf("build demo run: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("demo run: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("demo run status %d, want 200", resp.StatusCode)
	}

	demoAcct := idgen.Derive("account", "demo-onboarding")
	countBy := func(where string) int {
		return h.ClickHouseScalar(t, fmt.Sprintf(
			"SELECT count() FROM adtech.profile_signals WHERE account_id='%s' AND observed_at >= toDateTime('%s')%s",
			demoAcct, startTS, where))
	}

	// Wait for the demo's profile_signal (published by the ingest processor during
	// the synchronous run) to land in ClickHouse.
	deadline := time.Now().Add(30 * time.Second)
	for countBy("") < 1 {
		if time.Now().After(deadline) {
			t.Fatalf("demo profile_signal never landed in ClickHouse for account %s", demoAcct)
		}
		time.Sleep(2 * time.Second)
	}
	total := countBy("")

	// Every demo row carries the distinct ing_<32hex> ingest_trace_id (36 chars).
	if got := countBy(" AND startsWith(ingest_trace_id,'ing_') AND length(ingest_trace_id)=36"); got != total {
		t.Errorf("demo rows with well-formed ingest_trace_id = %d, want %d", got, total)
	}
	// And no demo row's trace_id carries the ingest format — the two never confuse.
	if got := countBy(" AND startsWith(trace_id,'ing_')"); got != 0 {
		t.Errorf("%d demo rows have an ing_-format trace_id (should be a real trace or empty)", got)
	}

	// The demo runs the REAL ingest path, so it emails the ingest-completion
	// notice to whoever ran it (the staff runner) — same as a customer upload
	// notifies its uploader.
	mailDeadline := time.Now().Add(20 * time.Second)
	for {
		msgs := h.MailpitSearch(t, email)
		if len(msgs) > 0 {
			if !strings.Contains(msgs[0].Subject, demoSegmentNameForMail) {
				t.Errorf("completion email subject = %q, want it to mention the demo segment", msgs[0].Subject)
			}
			break
		}
		if time.Now().After(mailDeadline) {
			t.Fatal("demo did not send an ingest-completion email to the runner")
		}
		time.Sleep(2 * time.Second)
	}
}

// demoSegmentNameForMail is the segment name the demo upload uses; the completion
// email subject includes it ("Audience upload \"Demo Newsletter\" succeeded").
const demoSegmentNameForMail = "Demo Newsletter"
