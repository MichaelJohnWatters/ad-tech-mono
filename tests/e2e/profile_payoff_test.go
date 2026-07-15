//go:build e2e

// Profile Store Phase 5 — the payoff valves:
//
//   - Segment EXPORT rides the report-jobs machinery: submit → the
//     report-runner renders the members to a CSV artifact in Minio → the
//     gateway streams the download. Ownership is the tenancy gate.
//   - The staff profile API assembles the whole picture for an id (cluster,
//     graph links, memberships with provenance, lake summary).
//   - The staff onboarding monitor lists drop-zone runs + provider rollups.
package e2e

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestSegmentExportJob(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "profile-export")
	uniq := fmt.Sprintf("exp-%d", time.Now().UnixNano())

	users := []string{uniq + "-u1", uniq + "-u2", uniq + "-u3"}
	segID := h.UploadAudience(t, w.AdvAcc.ID, uniq+"-list", "dsp_private", users)

	email := "export-" + w.AdvAcc.ID + "@e2e.local"
	h.CreateLoginUser(t, w.AdvAcc.ID, email, "e2e-pass", "owner")
	client := h.LoginAs(t, email, "e2e-pass")

	res := h.APIJSON(t, client, http.MethodPost, "/v1/api/reports/jobs", fmt.Sprintf(`{
		"name":"segment export — %s",
		"query_config":{"table":"segment_members","filters":{"segment_id":%q}},
		"format":"csv"
	}`, uniq, segID))
	jobID, _ := res["id"].(string)
	if jobID == "" {
		t.Fatalf("submit returned no job id: %v", res)
	}

	job := waitJobDone(t, h, client, jobID)
	if int(job["row_count"].(float64)) != len(users) {
		t.Errorf("row_count = %v, want %d", job["row_count"], len(users))
	}
	code, ctype, _, body := download(t, h, client, jobID)
	if code != http.StatusOK || !strings.Contains(ctype, "text/csv") {
		t.Fatalf("download status=%d ctype=%s", code, ctype)
	}
	for _, u := range users {
		if !strings.Contains(string(body), u) {
			t.Errorf("artifact missing member %s", u)
		}
	}
	if !strings.Contains(string(body), uniq+"-list") {
		t.Error("artifact missing segment_name column value")
	}

	// Tenancy: exporting a segment the account doesn't own is a 400.
	if code := h.APIStatus(t, client, http.MethodPost, "/v1/api/reports/jobs", `{
		"name":"steal","query_config":{"table":"segment_members","filters":{"segment_id":"00000000-0000-4000-8000-000000000000"}},"format":"csv"
	}`); code != http.StatusBadRequest {
		t.Errorf("foreign-segment export status = %d, want 400", code)
	}
}

func TestProfileAPIAndOnboardingMonitor(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "profile-api")
	uniq := time.Now().UnixNano()
	user := fmt.Sprintf("papi-user-%d", uniq)
	dev2 := fmt.Sprintf("papi-dev2-%d", uniq)

	h.AddIdentityEdge(t, user, dev2, "cross_device")
	segID := h.UploadAudience(t, w.AdvAcc.ID, fmt.Sprintf("papi-seg-%d", uniq), "public", []string{user})

	// Materialize the cluster (clustering needs only PG; lake jobs run too).
	runBuilder(t, h, lakeStore(t, h))

	// Staff session (admin has "*", covers support:read).
	admin := h.CreateAdmin(t, fmt.Sprintf("papi-admin-%d", uniq))
	adminEmail := fmt.Sprintf("papi-admin-%d@login.test", uniq)
	h.CreateLoginUser(t, admin.ID, adminEmail, "pw-e2e-1", "owner")
	staff := h.LoginAs(t, adminEmail, "pw-e2e-1")

	profile := h.APIJSON(t, staff, http.MethodGet, "/v1/api/profiles/"+user, "")
	if profile["person_id"] == nil || profile["person_id"] == "" {
		t.Errorf("profile has no person_id: %v", profile)
	}
	members := fmt.Sprint(profile["cluster_members"])
	if !strings.Contains(members, dev2) {
		t.Errorf("cluster_members %s missing %s", members, dev2)
	}
	memberships := fmt.Sprint(profile["memberships"])
	if !strings.Contains(memberships, segID) {
		t.Errorf("memberships missing segment %s: %s", segID, memberships)
	}

	// Onboarding monitor: seed one run row and read it back.
	provider := fmt.Sprintf("papi-prov-%d", uniq)
	if _, err := h.DB.Exec(`
INSERT INTO onboarding_runs (provider, file_key, account_id, status, total_rows, valid_rows, rejected_rows, matched_rows, match_rate, started_at)
VALUES ($1, $2, $3::uuid, 'completed', 10, 9, 1, 5, 0.55, now())`,
		provider, provider+"/incoming/x.csv", w.AdvAcc.ID); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	monitor := h.APIJSON(t, staff, http.MethodGet, "/v1/api/onboarding/runs?provider="+provider, "")
	if !strings.Contains(fmt.Sprint(monitor["runs"]), provider) {
		t.Errorf("monitor runs missing provider %s", provider)
	}
	if !strings.Contains(fmt.Sprint(monitor["providers"]), provider) {
		t.Errorf("monitor rollup missing provider %s", provider)
	}

	// Lake-state view (Batch runs page): staff sees per-table snapshot.
	if code := h.APIStatus(t, staff, http.MethodGet, "/v1/api/batch/lake", ""); code != http.StatusOK {
		t.Errorf("staff lake snapshot status = %d, want 200", code)
	}

	// Both surfaces are staff-only: an advertiser session gets 403.
	advEmail := fmt.Sprintf("papi-adv-%d@login.test", uniq)
	h.CreateLoginUser(t, w.AdvAcc.ID, advEmail, "pw-e2e-1", "owner")
	adv := h.LoginAs(t, advEmail, "pw-e2e-1")
	if code := h.APIStatus(t, adv, http.MethodGet, "/v1/api/profiles/"+user, ""); code != http.StatusForbidden {
		t.Errorf("advertiser profile lookup status = %d, want 403", code)
	}
	if code := h.APIStatus(t, adv, http.MethodGet, "/v1/api/onboarding/runs", ""); code != http.StatusForbidden {
		t.Errorf("advertiser onboarding monitor status = %d, want 403", code)
	}
	if code := h.APIStatus(t, adv, http.MethodGet, "/v1/api/batch/lake", ""); code != http.StatusForbidden {
		t.Errorf("advertiser lake snapshot status = %d, want 403", code)
	}
}
