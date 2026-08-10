//go:build e2e

package harness

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// AccountExportStatus is the test-side view of an export job.
type AccountExportStatus struct {
	Status        string `json:"status"`
	ArtifactBytes int64  `json:"artifact_bytes"`
	DownloadURL   string `json:"download_url"`
	Error         string `json:"error"`
}

// AccountExportRequest POSTs to enqueue an export and returns the status.
func (h *Harness) AccountExportRequest(t *testing.T, client *http.Client) AccountExportStatus {
	t.Helper()
	return h.accountExport(t, client, http.MethodPost, routes.APIAccountExport)
}

// AccountExportPoll GETs the current export status.
func (h *Harness) AccountExportPoll(t *testing.T, client *http.Client) AccountExportStatus {
	t.Helper()
	return h.accountExport(t, client, http.MethodGet, routes.APIAccountExport)
}

func (h *Harness) accountExport(t *testing.T, client *http.Client, method, url string) AccountExportStatus {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, method, h.URLs.Gateway+url, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("account export %s: %v", method, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("account export %s status %d: %s", method, resp.StatusCode, string(b))
	}
	var s AccountExportStatus
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("decode export status: %v (body=%s)", err, string(b))
	}
	return s
}

// AccountExportDownload GETs the export archive and returns the raw zip bytes.
func (h *Harness) AccountExportDownload(t *testing.T, client *http.Client) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, h.URLs.Gateway+routes.APIAccountExport+"/download", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("account export download: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("account export download status %d: %s", resp.StatusCode, string(b))
	}
	return b
}
