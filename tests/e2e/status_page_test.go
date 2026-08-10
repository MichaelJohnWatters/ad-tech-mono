//go:build e2e

// Public status page + staff incidents (PLAN Phase 11, item 107). The public
// /status page and /v1/api/status JSON are UNAUTHED and roll each service's
// /readyz up into customer-facing components; staff post incidents that surface
// on the page and floor the overall status.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

type statusReport struct {
	Overall     string `json:"overall"`
	OverallText string `json:"overall_text"`
	Components  []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Total  int    `json:"total_services"`
	} `json:"components"`
	ActiveIncidents []struct {
		Title  string `json:"title"`
		Impact string `json:"impact"`
	} `json:"active_incidents"`
}

func getStatusJSON(t *testing.T, h *harness.Harness) statusReport {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Public — a plain client, no auth.
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, h.URLs.Gateway+routes.APIStatus, nil)
	resp, err := harness.NewHTTPClient(10 * time.Second).Do(req)
	if err != nil {
		t.Fatalf("GET status: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status JSON %d: %s", resp.StatusCode, string(b))
	}
	var rep statusReport
	if err := json.Unmarshal(b, &rep); err != nil {
		t.Fatalf("decode status: %v (body=%s)", err, string(b))
	}
	return rep
}

func TestPublicStatusPageAndIncidents(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)

	// Public JSON: unauthed, 5 components, each backed by >=1 service.
	rep := getStatusJSON(t, h)
	if len(rep.Components) != 5 {
		t.Fatalf("components = %d, want 5", len(rep.Components))
	}
	for _, c := range rep.Components {
		if c.Total < 1 {
			t.Errorf("component %q has %d services, want >=1", c.Name, c.Total)
		}
	}
	if rep.Overall == "" {
		t.Errorf("overall empty")
	}

	// Public HTML page renders and is unauthed.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	preq, _ := http.NewRequestWithContext(ctx, http.MethodGet, h.URLs.Gateway+routes.StatusPage, nil)
	presp, err := harness.NewHTTPClient(10 * time.Second).Do(preq)
	if err != nil {
		t.Fatalf("GET /status page: %v", err)
	}
	body, _ := io.ReadAll(presp.Body)
	presp.Body.Close()
	if presp.StatusCode != 200 || !strings.Contains(string(body), "Components") {
		t.Fatalf("status page status=%d, missing 'Components' section", presp.StatusCode)
	}

	// Staff posts a CRITICAL incident → it must appear active + floor overall to
	// major_outage even though the stack itself is healthy.
	staff := h.CreateStaff(t, fmt.Sprintf("status-staff-%d", time.Now().UnixNano()))
	client := h.OwnerClient(t, staff.ID) // staff account + owner role → staff:owner (incidents:write)

	title := fmt.Sprintf("E2E outage %d", time.Now().UnixNano())
	created := postIncident(t, h, client, map[string]any{
		"title": title, "impact": "critical", "status": "investigating",
		"affected_components": []string{"Bidding & Auctions"},
	})
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatalf("incident create returned no id: %v", created)
	}

	rep = getStatusJSON(t, h)
	if rep.Overall != "major_outage" {
		t.Errorf("overall = %q, want major_outage (open critical incident)", rep.Overall)
	}
	found := false
	for _, inc := range rep.ActiveIncidents {
		if inc.Title == title {
			found = true
		}
	}
	if !found {
		t.Errorf("critical incident %q not in active_incidents", title)
	}

	// Resolve it → overall recovers, incident leaves the active list.
	postIncident(t, h, client, map[string]any{"id": id, "title": title, "impact": "critical", "status": "resolved"})
	rep = getStatusJSON(t, h)
	for _, inc := range rep.ActiveIncidents {
		if inc.Title == title {
			t.Errorf("resolved incident %q still active", title)
		}
	}
	if rep.Overall == "major_outage" {
		t.Errorf("overall still major_outage after resolving the only incident")
	}
}

func postIncident(t *testing.T, h *harness.Harness, client *http.Client, body map[string]any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.URLs.Gateway+routes.APIIncidents, strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST incident: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST incident %d: %s", resp.StatusCode, string(b))
	}
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}
