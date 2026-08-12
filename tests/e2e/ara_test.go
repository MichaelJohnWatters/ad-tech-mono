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
	req, _ := http.NewRequest(http.MethodGet, srcURL, nil)
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

	// 5) A report for an UNREGISTERED source/destination is accepted (200) but
	// dropped — never persisted under any account.
	orphan := `{"attribution_destination":"https://nobody.example","source_event_id":"99999999999","trigger_data":"1","report_id":"orphan-1"}`
	if code := post(ara.PathEventReport, orphan); code != http.StatusOK {
		t.Errorf("orphan report = %d, want 200 (accepted, dropped)", code)
	}
	var orphanRows int
	if err := h.DB.QueryRow(`SELECT count(*) FROM ara_reports WHERE report_id = 'orphan-1'`).Scan(&orphanRows); err != nil {
		t.Fatalf("count orphan: %v", err)
	}
	if orphanRows != 0 {
		t.Errorf("orphan report was persisted (%d rows) — must be dropped", orphanRows)
	}
}
