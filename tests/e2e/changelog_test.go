//go:build e2e

// API changelog (PLAN Phase 11 #109): staff publish API-change entries that surface
// on a PUBLIC feed (/v1/api/changelog) + page (/changelog) for external integrators,
// newest-release-first with breaking changes flagged. Staff CRUD is RBAC-gated
// (changelog:write); public reads are unauthed. Exercises the live gateway.
package e2e

import (
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

type clEntry struct {
	ID                string   `json:"id"`
	Version           string   `json:"version"`
	Category          string   `json:"category"`
	Breaking          bool     `json:"breaking"`
	Title             string   `json:"title"`
	AffectedEndpoints []string `json:"affected_endpoints"`
	CreatedBy         string   `json:"created_by"` // MUST stay empty on the public feed
}

func TestChangelogPublish(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "cl") // reset first, and gives us an advertiser
	uniq := fmt.Sprintf("%d", time.Now().UnixNano())

	staff := h.CreateStaff(t, "changelog-staff-"+uniq)
	staffCl := h.OwnerClient(t, staff.ID)

	do := func(cl *http.Client, method, body string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(method, h.URLs.Gateway+routes.APIChangelogEntries, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := cl.Do(req)
		if err != nil {
			t.Fatalf("%s entry: %v", method, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		return resp.StatusCode, m
	}

	// Staff publishes an older entry, then a newer BREAKING one; capture the newer id.
	if st, _ := do(staffCl, http.MethodPost, fmt.Sprintf(`{"version":"1.0.0-%s","release_date":"2026-01-01","category":"added","title":"Older %s"}`, uniq, uniq)); st != 200 {
		t.Fatalf("create older entry: %d", st)
	}
	st, res := do(staffCl, http.MethodPost, fmt.Sprintf(`{"version":"2.0.0-%s","release_date":"2026-06-01","category":"security","breaking":true,"title":"Newer %s","affected_endpoints":["/v1/api/campaigns"]}`, uniq, uniq))
	if st != 200 {
		t.Fatalf("create newer entry: %d", st)
	}
	newerID, _ := res["id"].(string)
	if newerID == "" {
		t.Fatal("create did not return an id")
	}

	// Input validation: a bad category is rejected 400.
	if st, _ := do(staffCl, http.MethodPost, fmt.Sprintf(`{"version":"3-%s","release_date":"2026-01-01","category":"bogus","title":"x"}`, uniq)); st != http.StatusBadRequest {
		t.Errorf("bad category: got %d, want 400", st)
	}

	feed := func() []clEntry {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, h.URLs.Gateway+routes.APIChangelog, nil)
		resp, err := h.HTTP.Do(req)
		if err != nil {
			t.Fatalf("public feed: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("public feed status %d", resp.StatusCode)
		}
		var f struct {
			Entries []clEntry `json:"entries"`
		}
		if err := json.Unmarshal(body, &f); err != nil {
			t.Fatalf("feed decode: %v (%s)", err, body)
		}
		return f.Entries
	}

	// Public feed: our two entries present, newest-first, breaking flagged, and
	// created_by NEVER leaked to the world-readable feed.
	entries := feed()
	newerIdx, olderIdx, breakingOK := -1, -1, false
	for i, e := range entries {
		if e.CreatedBy != "" {
			t.Errorf("public feed LEAKED created_by=%q (staff subject must not be public)", e.CreatedBy)
		}
		switch e.Version {
		case "2.0.0-" + uniq:
			newerIdx = i
			breakingOK = e.Breaking && e.Category == "security" && len(e.AffectedEndpoints) == 1
		case "1.0.0-" + uniq:
			olderIdx = i
		}
	}
	if newerIdx < 0 || olderIdx < 0 {
		t.Fatalf("published entries missing from public feed (newer=%d older=%d)", newerIdx, olderIdx)
	}
	if newerIdx > olderIdx {
		t.Errorf("feed not newest-first: newer at %d, older at %d", newerIdx, olderIdx)
	}
	if !breakingOK {
		t.Errorf("newer entry lost its breaking/category/affected_endpoints in the feed")
	}

	// Update the newer entry (POST with id) → the public feed reflects the new title.
	if st, _ := do(staffCl, http.MethodPost, fmt.Sprintf(`{"id":%q,"version":"2.0.1-%s","release_date":"2026-06-02","category":"fixed","title":"Edited %s"}`, newerID, uniq, uniq)); st != 200 {
		t.Fatalf("update entry: %d", st)
	}
	// Delete: a missing/non-UUID id is a 400 (not a 500), then delete the older entry.
	if st, _ := do(staffCl, http.MethodDelete, ""); st != http.StatusBadRequest {
		t.Errorf("delete with no id: got %d, want 400", st)
	}
	delReq, _ := http.NewRequest(http.MethodDelete, h.URLs.Gateway+routes.APIChangelogEntries+"?id="+olderIDOf(entries, "1.0.0-"+uniq), nil)
	delResp, err := staffCl.Do(delReq)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	delResp.Body.Close()
	if delResp.StatusCode != 200 {
		t.Fatalf("delete older entry: %d", delResp.StatusCode)
	}

	after := feed()
	var sawEdited, sawOld bool
	for _, e := range after {
		if e.Version == "2.0.1-"+uniq && e.Title == "Edited "+uniq {
			sawEdited = true
		}
		if e.Version == "1.0.0-"+uniq {
			sawOld = true
		}
	}
	if !sawEdited {
		t.Errorf("edited entry not reflected in feed")
	}
	if sawOld {
		t.Errorf("deleted entry still in feed")
	}

	// Public HTML page renders an entry.
	pReq, _ := http.NewRequest(http.MethodGet, h.URLs.Gateway+routes.Changelog, nil)
	pResp, err := h.HTTP.Do(pReq)
	if err != nil {
		t.Fatalf("public page: %v", err)
	}
	pBody, _ := io.ReadAll(pResp.Body)
	pResp.Body.Close()
	if !strings.Contains(string(pBody), "Edited "+uniq) {
		t.Errorf("public /changelog page did not render the entry")
	}

	// Negative RBAC: an advertiser owner (no changelog:write) cannot publish.
	advCl := h.OwnerClient(t, w.AdvAcc.ID)
	if st, _ := do(advCl, http.MethodPost, fmt.Sprintf(`{"version":"9.9.9-%s","release_date":"2026-01-01","title":"nope"}`, uniq)); st != http.StatusForbidden {
		t.Errorf("advertiser publish: got %d, want 403", st)
	}
}

func olderIDOf(entries []clEntry, version string) string {
	for _, e := range entries {
		if e.Version == version {
			return e.ID
		}
	}
	return ""
}
