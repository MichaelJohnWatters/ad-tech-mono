//go:build e2e

package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// Helpers for the hot/cold tiering long-run test (tiering_test.go). They read
// the reporting query API (the tiered store under test), the pipeline's lake
// snapshot (the cold write path), and the reporting deployment env (to learn the
// live hot_window + whether tiering is enabled at all).

// ReportImpressionCountSince POSTs a count query to the reporting query API
// (the same endpoint the gateway proxies) scoped to time_from, and returns the
// scalar count. This routes through the TieredStore, so the answer comes from
// ClickHouse (hot), the lake (cold), or a merge, depending on the range.
func (h *Harness) ReportImpressionCountSince(t *testing.T, from time.Time) int {
	t.Helper()
	return h.reportCount(t, map[string]interface{}{
		"table":     "impressions",
		"metrics":   []string{"count"},
		"time_from": from.UTC().Format(time.RFC3339),
	})
}

func (h *Harness) reportCount(t *testing.T, body map[string]interface{}) int {
	t.Helper()
	raw, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URLs.Reporting+routes.ReportingQuery, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("reporting query request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("reporting query: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reporting query status %d for %s", resp.StatusCode, string(raw))
	}
	var out struct {
		Columns []string        `json:"columns"`
		Rows    [][]interface{} `json:"rows"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode reporting query: %v", err)
	}
	if len(out.Rows) == 0 || len(out.Rows[0]) == 0 {
		return 0
	}
	switch v := out.Rows[0][0].(type) {
	case float64:
		return int(v)
	case string:
		var n int
		_, _ = fmt.Sscanf(v, "%d", &n)
		return n
	default:
		t.Fatalf("unexpected count type %T", out.Rows[0][0])
		return 0
	}
}

// LakeRows returns the pipeline datalake snapshot's total_rows for a table — the
// cold write path's own count (source of truth for what physically reached the
// Parquet/Delta lake). Snapshot flushes first, so the number is consistent.
func (h *Harness) LakeRows(t *testing.T, table string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	url := routes.DefaultPipelineURL + "/debug/datalake/snapshot?table=" + table
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("pipeline snapshot: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0
	}
	var out map[string]struct {
		TotalRows int `json:"total_rows"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	return out[table].TotalRows
}

// ReportingEnvVar reads an env value from the running reporting deployment via
// kubectl (used to learn the live hot_window and whether tiering is enabled).
// Returns ("", false) if absent.
func (h *Harness) ReportingEnvVar(t *testing.T, name string) (string, bool) {
	t.Helper()
	jsonpath := fmt.Sprintf(`jsonpath={.spec.template.spec.containers[0].env[?(@.name=="%s")].value}`, name)
	out, err := exec.Command("kubectl", "get", "deployment", "reporting", "-n", "adtech", "-o", jsonpath).Output()
	if err != nil {
		return "", false
	}
	v := strings.TrimSpace(string(out))
	if v == "" {
		return "", false
	}
	return v, true
}
