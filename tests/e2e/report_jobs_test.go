//go:build e2e

// Ad-hoc report jobs: submit → live worker executes → download the artifact
// through the gateway, asserting the artifact agrees with the sync query
// path. Also: parquet output, tenant isolation (cross-account 404), and
// validation (bad format 400).
package e2e

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// accountIDByEmail resolves the accounts row a signup created.
func accountIDByEmail(t *testing.T, h *harness.Harness, email string) string {
	t.Helper()
	var id string
	if err := h.DB.QueryRow(`SELECT id::text FROM accounts WHERE email = $1`, email).Scan(&id); err != nil {
		t.Fatalf("account for %s: %v", email, err)
	}
	return id
}

// download fetches a report-job artifact and returns status, content type,
// disposition and body.
func download(t *testing.T, h *harness.Harness, client *http.Client, jobID string) (int, string, string, []byte) {
	t.Helper()
	resp, err := client.Get(h.URLs.Gateway + "/v1/api/reports/jobs/" + jobID + "/download")
	if err != nil {
		t.Fatalf("download %s: %v", jobID, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Content-Type"), resp.Header.Get("Content-Disposition"), body
}

// waitJobDone polls the job until done (fails fast on failed) and returns it.
func waitJobDone(t *testing.T, h *harness.Harness, client *http.Client, jobID string) map[string]any {
	t.Helper()
	var job map[string]any
	harness.WaitFor(t, 60*time.Second, "report job "+jobID+" done", func() bool {
		job = h.APIJSON(t, client, http.MethodGet, "/v1/api/reports/jobs/"+jobID, "")
		switch job["status"] {
		case "done":
			return true
		case "failed":
			t.Fatalf("job %s failed: %v", jobID, job["error"])
		}
		return false
	})
	return job
}

func TestReportJobsEndToEnd(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)

	uniq := fmt.Sprintf("rj-%d", time.Now().UnixNano())
	advEmail := uniq + "-adv@api.test"
	adv := h.Signup(t, "RJ Adv", advEmail, "pw-e2e-1", "advertiser")
	advID := accountIDByEmail(t, h, advEmail)

	// Seed analytics: three impressions attributed to this account. Ingestion
	// is async (tracker → NATS → reporting), so wait for the sync query to see
	// them before submitting the job — then the artifact must agree.
	const imps = 3
	for i := 0; i < imps; i++ {
		h.FireImpression(t, fmt.Sprintf("%032x", time.Now().UnixNano()+int64(i)),
			"rj-camp", "rj-cr", "rj-pl", "rj-pub", advID, "USD", 2.5)
	}
	query := `{"table":"impressions","metrics":["count"],"time_from":"` +
		time.Now().Add(-time.Hour).UTC().Format(time.RFC3339) + `"}`
	harness.WaitFor(t, 30*time.Second, "impressions visible to the sync report path", func() bool {
		res := h.APIJSON(t, adv, http.MethodPost, "/v1/api/reports", query)
		rows, _ := res["rows"].([]any)
		if len(rows) == 0 {
			return false
		}
		row, _ := rows[0].([]any)
		return len(row) > 0 && row[0] == float64(imps)
	})

	t.Run("csv_job_matches_sync_query", func(t *testing.T) {
		created := h.APIJSON(t, adv, http.MethodPost, "/v1/api/reports/jobs",
			`{"name":"rj csv","query_config":`+query+`,"format":"csv"}`)
		jobID, _ := created["id"].(string)
		if jobID == "" {
			t.Fatalf("submit returned no id: %v", created)
		}
		job := waitJobDone(t, h, adv, jobID)
		if rc, _ := job["row_count"].(float64); rc != 1 {
			t.Errorf("row_count = %v, want 1 aggregate row", job["row_count"])
		}

		status, ctype, cdisp, body := download(t, h, adv, jobID)
		if status != http.StatusOK || ctype != "text/csv" {
			t.Fatalf("download status=%d type=%s body=%s", status, ctype, body)
		}
		if !strings.Contains(cdisp, `attachment; filename="rj-csv.csv"`) {
			t.Errorf("content-disposition: %q", cdisp)
		}
		want := fmt.Sprintf("count\n%d\n", imps)
		if string(body) != want {
			t.Errorf("artifact = %q, want %q (must agree with the sync query)", body, want)
		}
	})

	t.Run("parquet_job_produces_parquet", func(t *testing.T) {
		created := h.APIJSON(t, adv, http.MethodPost, "/v1/api/reports/jobs",
			`{"name":"rj parquet","query_config":`+query+`,"format":"parquet"}`)
		jobID, _ := created["id"].(string)
		waitJobDone(t, h, adv, jobID)
		status, ctype, _, body := download(t, h, adv, jobID)
		if status != http.StatusOK {
			t.Fatalf("download status=%d", status)
		}
		if !strings.Contains(ctype, "parquet") {
			t.Errorf("content-type %q", ctype)
		}
		if !bytes.HasPrefix(body, []byte("PAR1")) {
			t.Errorf("artifact does not start with the parquet magic (got %q…)", body[:min(8, len(body))])
		}
	})

	t.Run("cross_tenant_job_is_invisible", func(t *testing.T) {
		created := h.APIJSON(t, adv, http.MethodPost, "/v1/api/reports/jobs",
			`{"name":"rj private","query_config":`+query+`}`)
		jobID, _ := created["id"].(string)

		other := h.Signup(t, "RJ Other", uniq+"-other@api.test", "pw-e2e-1", "advertiser")
		if got := h.APIStatus(t, other, http.MethodGet, "/v1/api/reports/jobs/"+jobID, ""); got != http.StatusNotFound {
			t.Errorf("cross-tenant status get = %d, want 404", got)
		}
		if got := h.APIStatus(t, other, http.MethodGet, "/v1/api/reports/jobs/"+jobID+"/download", ""); got != http.StatusNotFound {
			t.Errorf("cross-tenant download = %d, want 404", got)
		}
		res := h.APIJSON(t, other, http.MethodGet, "/v1/api/reports/jobs", "")
		if items, _ := res["items"].([]any); len(items) != 0 {
			t.Errorf("other account sees %d jobs, want 0", len(items))
		}
	})

	t.Run("bad_format_rejected", func(t *testing.T) {
		if got := h.APIStatus(t, adv, http.MethodPost, "/v1/api/reports/jobs",
			`{"name":"x","query_config":`+query+`,"format":"xlsx"}`); got != http.StatusBadRequest {
			t.Errorf("bad format = %d, want 400", got)
		}
	})
}
