//go:build e2e

// Privacy Sandbox ARA (Attribution Reporting API) — the REAL, inspectable parts,
// end to end against the live tracker (the ARA reporting origin). What this
// proves: source registration returns a well-formed Attribution-Reporting-
// Register-Source header AND records the source; a conversion returns a
// Register-Trigger header; the well-known report-ingest endpoint resolves the
// owning advertiser account for a browser-POSTed report and persists it to the
// SEPARATE ara_reports table (idempotently); an unresolvable report is accepted
// but dropped.
//
// MOCK BOUNDARY (NOT tested here — a real Privacy-Sandbox browser only): the
// source↔trigger match, the k-anonymity noise, the multi-day delay, the
// aggregation-service decrypt. The harness plays the browser ONLY to the extent
// of POSTing a sample report to the ingest endpoint — never simulating the match.
package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adserving"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/ara"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestARARegistrationAndReportIngest(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "ara")

	const pod = "tracker-0" // all tracker replicas share this config pod-id
	t.Cleanup(func() { h.SetConfigForPod(t, "tracker.ara_enabled", "false", pod) })
	h.SetConfigForPod(t, "tracker.ara_enabled", "true", pod)

	client := harness.NewHTTPClient(10 * time.Second)
	const dest = "https://advertiser.example"

	// 1) Source registration returns a well-formed Register-Source header.
	srcURL := fmt.Sprintf("%s%s?advid=%s&dest=%s&cid=%s",
		h.URLs.Tracker, "/v1/t/ara/src", w.AdvAcc.ID, dest, w.Placement.ID)

	// Enforcement: an UNSIGNED source beacon is rejected (strict signing on the
	// stack) — advid can't be forged into an ara_sources row for any account.
	ureq, _ := http.NewRequest(http.MethodGet, srcURL, nil)
	uresp, err := client.Do(ureq)
	if err != nil {
		t.Fatalf("unsigned source beacon: %v", err)
	}
	ucode := uresp.StatusCode
	uresp.Body.Close()
	if ucode != http.StatusForbidden {
		t.Errorf("unsigned source beacon = %d, want 403 (signature must be enforced)", ucode)
	}

	// Enforcement: a SIGNED-but-EXPIRED source beacon is rejected (410) — a
	// captured beacon can't be replayed past its window (exp_validation is on).
	expired := srcURL + "&exp=" + strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10)
	ereq, _ := http.NewRequest(http.MethodGet, adserving.SignURL(expired, adserving.DefaultSigningKey), nil)
	eresp, err := client.Do(ereq)
	if err != nil {
		t.Fatalf("expired source beacon: %v", err)
	}
	ecode := eresp.StatusCode
	eresp.Body.Close()
	if ecode != http.StatusGone {
		t.Errorf("expired source beacon = %d, want 410 (replay window enforced)", ecode)
	}

	// Signed like the ad server → registers.
	req, _ := http.NewRequest(http.MethodGet, adserving.SignURL(srcURL, adserving.DefaultSigningKey), nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("source beacon: %v", err)
	}
	srcHdr := resp.Header.Get(ara.HeaderRegisterSource)
	resp.Body.Close()
	if srcHdr == "" {
		t.Fatal("no Attribution-Reporting-Register-Source header on the source beacon")
	}
	var src map[string]any
	if err := json.Unmarshal([]byte(srcHdr), &src); err != nil {
		t.Fatalf("register-source header is not valid JSON: %v", err)
	}
	if src["destination"] != dest {
		t.Errorf("register-source destination = %v, want %s", src["destination"], dest)
	}
	sourceEventID, _ := src["source_event_id"].(string)
	if sourceEventID == "" {
		t.Fatal("register-source header has no source_event_id")
	}

	// 2) A conversion returns a well-formed Register-Trigger header. The tracker
	// enforces signature_validation on this stack, so sign like the ad server;
	// a browser-shaped UA avoids the real-time fraud bot-drop.
	convURL := fmt.Sprintf("%s/v1/t/conv?tid=%s&type=purchase&rev=42.00&cur=USD&advid=%s",
		h.URLs.Tracker, "ara-conv-"+sourceEventID, w.AdvAcc.ID)
	creq, _ := http.NewRequest(http.MethodGet, adserving.SignURL(convURL, adserving.DefaultSigningKey), nil)
	creq.Header.Set("User-Agent", "Mozilla/5.0 (e2e-harness)")
	creq.Header.Set("Referer", "https://publisher.example/article")
	cresp, err := client.Do(creq)
	if err != nil {
		t.Fatalf("conversion: %v", err)
	}
	trigHdr := cresp.Header.Get(ara.HeaderRegisterTrigger)
	cresp.Body.Close()
	if trigHdr == "" {
		t.Fatal("no Attribution-Reporting-Register-Trigger header on the conversion")
	}
	if !json.Valid([]byte(trigHdr)) {
		t.Errorf("register-trigger header is not valid JSON: %s", trigHdr)
	}

	// 3) The browser POSTs an event-level report to the well-known endpoint; the
	// tracker resolves the account by source_event_id and persists it.
	reportID := "report-" + sourceEventID
	body := fmt.Sprintf(`{"attribution_destination":%q,"source_event_id":%q,"trigger_data":"1","source_type":"navigation","report_id":%q,"randomized_trigger_rate":0.0024}`,
		dest, sourceEventID, reportID)
	post := func(path, body string) int {
		r, _ := http.NewRequest(http.MethodPost, h.URLs.Tracker+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(r)
		if err != nil {
			t.Fatalf("post %s: %v", path, err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post(ara.PathEventReport, body); code != http.StatusOK {
		t.Fatalf("event report ingest = %d, want 200", code)
	}

	// Persisted under the advertiser account, in the SEPARATE ara_reports table.
	countReports := func() int {
		var n int
		if err := h.DB.QueryRow(
			`SELECT count(*) FROM ara_reports WHERE account_id = $1::uuid AND report_type = 'event' AND report_id = $2`,
			w.AdvAcc.ID, reportID).Scan(&n); err != nil {
			t.Fatalf("count ara_reports: %v", err)
		}
		return n
	}
	if got := countReports(); got != 1 {
		t.Fatalf("ara_reports rows after ingest = %d, want 1", got)
	}

	// 4) Idempotent: the browser may retry delivery — same report_id → no dup.
	if code := post(ara.PathEventReport, body); code != http.StatusOK {
		t.Fatalf("retried event report = %d, want 200", code)
	}
	if got := countReports(); got != 1 {
		t.Errorf("ara_reports rows after retry = %d, want 1 (dedup)", got)
	}

	// 4b) The advertiser reads its overlay back through the account-scoped API.
	email := "ara-adv-" + sourceEventID + "@api.test"
	h.CreateLoginUser(t, w.AdvAcc.ID, email, "pw-e2e-1", "owner")
	adv := h.LoginAs(t, email, "pw-e2e-1")
	areq, _ := http.NewRequest(http.MethodGet, h.URLs.Gateway+routes.APIARAReports, nil)
	aresp, err := adv.Do(areq)
	if err != nil {
		t.Fatalf("read ara reports: %v", err)
	}
	if aresp.StatusCode != http.StatusOK {
		aresp.Body.Close()
		t.Fatalf("GET %s = %d, want 200", routes.APIARAReports, aresp.StatusCode)
	}
	var out struct {
		Summary struct {
			Event int `json:"event"`
		} `json:"summary"`
	}
	derr := json.NewDecoder(aresp.Body).Decode(&out)
	aresp.Body.Close()
	if derr != nil {
		t.Fatalf("decode ara reports: %v", derr)
	}
	if out.Summary.Event < 1 {
		t.Errorf("advertiser ARA summary.event = %d, want >= 1 (the ingested report)", out.Summary.Event)
	}

	// 5) POISONING ATTEMPT: an event report claiming the victim's REAL destination
	// but a source_event_id the victim never registered must NOT land in the
	// victim's account — event reports resolve ONLY by the unguessable source id,
	// never by the (public) destination.
	poison := fmt.Sprintf(`{"attribution_destination":%q,"source_event_id":"99999999999","trigger_data":"1","report_id":"poison-1"}`, dest)
	if code := post(ara.PathEventReport, poison); code != http.StatusOK {
		t.Errorf("poison report = %d, want 200 (accepted, dropped)", code)
	}
	var poisonRows int
	if err := h.DB.QueryRow(`SELECT count(*) FROM ara_reports WHERE report_id = 'poison-1'`).Scan(&poisonRows); err != nil {
		t.Fatalf("count poison: %v", err)
	}
	if poisonRows != 0 {
		t.Errorf("poison report persisted (%d rows) — an event report was resolved by destination, not source id", poisonRows)
	}

	// 6) AGGREGATABLE POISONING (F1): an aggregatable report names the victim's REAL
	// (public) destination but carries no source id. It must NOT be attributed to
	// the victim — aggregatable reports never resolve to a tenant; they land in the
	// platform quarantine (staff-only, never in an advertiser overlay).
	aggBody := fmt.Sprintf(`{"attribution_destination":%q,"report_id":"agg-poison-1","aggregation_service_payloads":[{"payload":"encrypted-opaque"}]}`, dest)
	if code := post(ara.PathAggregateReport, aggBody); code != http.StatusOK {
		t.Errorf("aggregatable report = %d, want 200 (accepted, quarantined)", code)
	}
	var aggInTenant int
	if err := h.DB.QueryRow(
		`SELECT count(*) FROM ara_reports WHERE account_id = $1::uuid AND report_type = 'aggregate'`,
		w.AdvAcc.ID).Scan(&aggInTenant); err != nil {
		t.Fatalf("count tenant aggregate: %v", err)
	}
	if aggInTenant != 0 {
		t.Errorf("aggregatable report landed in the victim's ara_reports (%d rows) — cross-tenant write NOT closed", aggInTenant)
	}
	countQuarantine := func() int {
		var n int
		if err := h.DB.QueryRow(
			`SELECT count(*) FROM ara_aggregatable_quarantine WHERE report_id = 'agg-poison-1'`).Scan(&n); err != nil {
			t.Fatalf("count quarantine: %v", err)
		}
		return n
	}
	if got := countQuarantine(); got != 1 {
		t.Errorf("aggregatable report not quarantined (%d rows), want 1", got)
	}
	// Idempotent: a browser retry (same report_id) doesn't duplicate.
	if code := post(ara.PathAggregateReport, aggBody); code != http.StatusOK {
		t.Errorf("retried aggregatable report = %d, want 200", code)
	}
	if got := countQuarantine(); got != 1 {
		t.Errorf("quarantine rows after retry = %d, want 1 (dedup)", got)
	}
}
