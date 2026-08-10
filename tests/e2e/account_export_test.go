//go:build e2e

// Account Closure and Data Export (PLAN Phase 11, item 105) — slice 2: the data
// export package. An owner requests an export; the report-runner worker zips the
// account's data (campaigns, creatives, audiences aggregate, invoices, payouts,
// audit log) into the private reports bucket; the gateway streams the download.
package e2e

import (
	"archive/zip"
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestAccountDataExport(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "acct-export")
	adv := w.AdvAcc
	client := h.OwnerClient(t, adv.ID)

	// No export yet.
	if s := h.AccountExportPoll(t, client); s.Status != "none" {
		t.Fatalf("expected no export initially, got %q", s.Status)
	}

	// Request one → queued or running.
	req := h.AccountExportRequest(t, client)
	if req.Status != "queued" && req.Status != "running" && req.Status != "done" {
		t.Fatalf("export request status = %q, want queued/running/done", req.Status)
	}

	// The report-runner worker drains it → done with a download URL.
	var done harness.AccountExportStatus
	harness.WaitFor(t, 30*time.Second, "account export completes", func() bool {
		done = h.AccountExportPoll(t, client)
		if done.Status == "failed" {
			t.Fatalf("export failed: %s", done.Error)
		}
		return done.Status == "done" && done.DownloadURL != ""
	})
	if done.ArtifactBytes <= 0 {
		t.Errorf("export artifact_bytes = %d, want > 0", done.ArtifactBytes)
	}

	// Download + unzip the archive; assert the expected members and that the
	// account's campaign appears in campaigns.csv.
	raw := h.AccountExportDownload(t, client)
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("open export zip (%d bytes): %v", len(raw), err)
	}
	files := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open %s: %v", f.Name, err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		files[f.Name] = string(b)
	}
	for _, want := range []string{"metadata.json", "campaigns.csv", "creatives.csv", "audiences.csv", "invoices.csv", "payouts.csv", "audit_log.csv"} {
		if _, ok := files[want]; !ok {
			t.Errorf("export zip missing %s (has %v)", want, keysOf(files))
		}
	}
	// The world's campaign is named "e2e-li-acct-export" — it must be in the CSV.
	if !strings.Contains(files["campaigns.csv"], "e2e-li-acct-export") {
		t.Errorf("campaigns.csv missing the account's campaign:\n%s", files["campaigns.csv"])
	}
	// metadata.json carries the account id and the privacy note.
	if !strings.Contains(files["metadata.json"], adv.ID) {
		t.Errorf("metadata.json missing account id")
	}
	if !strings.Contains(files["metadata.json"], "no member ids") {
		t.Errorf("metadata.json missing the aggregate-only privacy note")
	}

	// The export job is recorded done with an artifact key.
	var status, key string
	if err := h.DB.QueryRow(
		`SELECT status, COALESCE(artifact_key,'') FROM account_export_jobs WHERE account_id = $1::uuid ORDER BY created_at DESC LIMIT 1`,
		adv.ID).Scan(&status, &key); err != nil {
		t.Fatalf("export job row: %v", err)
	}
	if status != "done" || key == "" {
		t.Errorf("export job status=%q key=%q, want done + non-empty key", status, key)
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
