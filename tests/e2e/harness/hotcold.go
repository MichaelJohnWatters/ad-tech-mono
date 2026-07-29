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

// Helpers for the hot/cold store long-run test (hot/cold storage_test.go). They read
// the reporting query API (the hot/cold store under test), the pipeline's lake
// snapshot (the cold write path), and the reporting deployment env (to learn the
// live hot_window + whether hot/cold storage is enabled at all).

// ClickHouseScalar runs a scalar-returning query against ClickHouse and returns
// the integer result. Used to assert an event reached the analytical store (the
// single source of truth since ADR 0006) without waiting on the hourly export.
func (h *Harness) ClickHouseScalar(t *testing.T, query string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := h.clickhouseQuery(ctx, query)
	if err != nil {
		t.Fatalf("clickhouse scalar query: %v", err)
	}
	var n int
	_, _ = fmt.Sscanf(strings.TrimSpace(out), "%d", &n)
	return n
}

// TriggerExport forces the ClickHouse→Parquet export for the hour containing
// `when` (ADR 0006 phase 4), so a freshly-fired burst is in the derived archive
// before a cold read reaches for it. The export is hourly in production; tests
// can't wait an hour, so they drive it directly. Idempotent (overwrites the hour).
func (h *Harness) TriggerExport(t *testing.T, when time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	hour := when.UTC().Truncate(time.Hour).Format(time.RFC3339)
	url := h.URLs.Reporting + routes.ReportingExportRun + "?hours=1&hour=" + hour
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		t.Fatalf("export request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("trigger export: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("trigger export: HTTP %d", resp.StatusCode)
	}
}

// ReportViewabilityRate POSTs a viewability_rate query to the reporting API for
// one publisher since `from`, optionally scoped to a channel ("video"/"display"/
// ""), and returns (rate%, ok). ok=false means the metric was null (no
// impressions in scope). Exercises the full derived-metric path
// (views.sum_viewable / impressions.count) over live ClickHouse.
func (h *Harness) ReportViewabilityRate(t *testing.T, publisherID, channel string, from time.Time) (float64, bool) {
	t.Helper()
	filters := map[string]string{"publisher_id": publisherID}
	if channel != "" {
		filters["channel"] = channel
	}
	body, _ := json.Marshal(map[string]interface{}{
		"table":     "impressions",
		"metrics":   []string{"viewability_rate"},
		"filters":   filters,
		"time_from": from.UTC().Format(time.RFC3339),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.URLs.Reporting+routes.ReportingQuery, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("viewability query: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("viewability query status %d", resp.StatusCode)
	}
	var out struct {
		Columns []string        `json:"columns"`
		Rows    [][]interface{} `json:"rows"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode viewability query: %v", err)
	}
	if len(out.Rows) == 0 {
		return 0, false
	}
	for i, c := range out.Columns {
		if c == "viewability_rate" {
			v := out.Rows[0][i]
			if v == nil {
				return 0, false
			}
			if f, ok := v.(float64); ok {
				return f, true
			}
		}
	}
	return 0, false
}

// ReportRow POSTs a query to the reporting API (table impressions, scoped to
// filters + time_from) and returns the requested metrics as a name→value map for
// the single result row. Numeric metrics arrive as float64; a nil/absent metric
// is omitted. Used to assert per-publisher isolation (count) and money split
// (net_revenue vs gross) over the live analytics backend.
func (h *Harness) ReportRow(t *testing.T, filters map[string]string, metrics []string, from time.Time) map[string]float64 {
	t.Helper()
	body, _ := json.Marshal(map[string]interface{}{
		"table":     "impressions",
		"metrics":   metrics,
		"filters":   filters,
		"time_from": from.UTC().Format(time.RFC3339),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.URLs.Reporting+routes.ReportingQuery, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("report row query: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("report row query status %d", resp.StatusCode)
	}
	var out struct {
		Columns []string        `json:"columns"`
		Rows    [][]interface{} `json:"rows"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode report row: %v", err)
	}
	res := map[string]float64{}
	if len(out.Rows) == 0 {
		return res
	}
	for i, c := range out.Columns {
		if i >= len(out.Rows[0]) {
			continue
		}
		switch v := out.Rows[0][i].(type) {
		case float64:
			res[c] = v
		case string:
			var f float64
			_, _ = fmt.Sscanf(v, "%g", &f)
			res[c] = f
		}
	}
	return res
}

// ReportImpressionCountSince POSTs a count query to the reporting query API
// (the same endpoint the gateway proxies) scoped to time_from, and returns the
// scalar count. This routes through the HotColdStore, so the answer comes from
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

// LakeRows returns the exported total_rows for a table from reporting's export
// snapshot — the count of what physically reached the derived Parquet archive
// (ADR 0006 phase 5; replaced the retired pipeline Delta snapshot). Note: the
// export is hourly, so callers must trigger an export (POST /debug/export/run)
// before expecting the current hour's rows to be reflected.
func (h *Harness) LakeRows(t *testing.T, table string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	url := routes.DefaultReportingURL + routes.ReportingExportSnapshot
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("reporting export snapshot: %v", err)
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
// kubectl (used to learn the live hot_window and whether hot/cold storage is enabled).
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
