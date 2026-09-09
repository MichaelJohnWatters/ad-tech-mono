//go:build e2e

// The nightly flight-transition job (cmd/dayboundary) has a unit test but no
// e2e — yet a silent no-op here would leave expired campaigns serving forever.
// This drives it exactly as the CronJob does (`go run ./cmd/dayboundary`) and
// proves a campaign whose flight has ended is cascaded to 'ended' (IO
// active→ended, line item live→ended), and that a re-run is idempotent.
package e2e

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestDayBoundaryEndsExpiredFlights(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.Reset(t)

	uniq := fmt.Sprintf("dayb-%d", time.Now().UnixNano())
	adv := h.Signup(t, "DayB Adv", uniq+"@api.test", "pw-e2e-1", "advertiser")

	// A display campaign opens live (IO status active, line item status live)
	// with a flight window that has already ended relative to the processed
	// date below. Nothing transitions it until the day-boundary job runs — that
	// is exactly the gap the job closes.
	created := h.APIJSON(t, adv, http.MethodPost, "/v1/api/campaigns", `{
		"name":"Expired Flight",
		"base_bid":1.50,
		"daily_budget":100,
		"bid_strategy":"cpm",
		"start_date":"2026-07-01",
		"end_date":"2026-07-20"
	}`)
	campaignID, _ := created["id"].(string)
	if campaignID == "" {
		t.Fatalf("create returned no id: %v", created)
	}
	if got := lineItemStatus(t, h, campaignID); got != "live" {
		t.Fatalf("new display campaign status = %q, want live", got)
	}

	// Run the job as the NOBYPASSRLS app role — the SAME role the in-cluster
	// CronJob connects with. The harness superuser DSN bypasses RLS and MASKED
	// a real bug here: without the platform hatch the job silently no-ops
	// under adtech_app (0 rows matched, exit 0). Never "fix" this back to
	// h.URLs.PostgresURL.
	appDSN := appRoleDSN(t, h.URLs.PostgresURL)
	runDayBoundary := func(date string) {
		t.Helper()
		cmd := exec.Command("go", "run", "./cmd/dayboundary", "--date", date)
		cmd.Dir = "../.."
		// dayboundary hard-exits without DATABASE_URL; pass the DSN explicitly
		// rather than relying on it being in the caller's env.
		cmd.Env = append(os.Environ(), "DATABASE_URL="+appDSN)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("dayboundary --date %s failed: %v\n%s", date, err, out)
		}
	}

	// Process a date AFTER the flight's end_date (2026-07-20) → EndFlights fires.
	runDayBoundary("2026-07-25")
	if got := lineItemStatus(t, h, campaignID); got != "ended" {
		t.Errorf("after day boundary, line item status = %q, want ended (flight ended 2026-07-20)", got)
	}

	// Idempotent — re-running the same boundary is safe and leaves it ended
	// (the job's headline contract: "safe to re-run").
	runDayBoundary("2026-07-25")
	if got := lineItemStatus(t, h, campaignID); got != "ended" {
		t.Errorf("after re-run, line item status = %q, want ended (job must be idempotent)", got)
	}
}

// appRoleDSN derives the adtech_app (NOBYPASSRLS) DSN from the harness owner
// URL — the same credential swap as rls_test.go's openAppRoleDB, but returning
// the DSN for a subprocess. Override wholesale with E2E_APP_POSTGRES_URL.
// Unlike openAppRoleDB this FAILS rather than skips: running dayboundary as
// the owner role would bypass RLS and mask the exact bug this test guards.
func appRoleDSN(t *testing.T, ownerURL string) string {
	t.Helper()
	if v := os.Getenv("E2E_APP_POSTGRES_URL"); v != "" {
		return v
	}
	u, err := url.Parse(ownerURL)
	if err != nil {
		t.Fatalf("parse owner postgres URL: %v", err)
	}
	u.User = url.UserPassword("adtech_app", "adtech-app-local")
	return u.String()
}

// lineItemStatus reads a line item's status directly (h.DB is the BYPASSRLS dev
// role, so no tenant GUC needed). campaign_id is the line item id everywhere.
func lineItemStatus(t *testing.T, h *harness.Harness, id string) string {
	t.Helper()
	var status string
	if err := h.DB.QueryRow(`SELECT status FROM line_items WHERE id = $1::uuid`, id).Scan(&status); err != nil {
		if err == sql.ErrNoRows {
			t.Fatalf("line item %s not found", id)
		}
		t.Fatalf("line item %s status: %v", id, err)
	}
	return status
}
