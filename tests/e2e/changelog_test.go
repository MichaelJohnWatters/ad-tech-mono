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

func TestChangelogPublish(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "cl") // reset first, and gives us an advertiser
	uniq := fmt.Sprintf("%d", time.Now().UnixNano())

	staff := h.CreateStaff(t, "changelog-staff-"+uniq)
	staffCl := h.OwnerClient(t, staff.ID)

	post := func(cl *http.Client, body string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, h.URLs.Gateway+routes.APIChangelogEntries, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := cl.Do(req)
		if err != nil {
			t.Fatalf("post entry: %v", err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}

	// Staff publishes an older entry, then a newer BREAKING one.
	if st := post(staffCl, fmt.Sprintf(`{"version":"1.0.0-%s","release_date":"2026-01-01","category":"added","title":"Older %s"}`, uniq, uniq)); st != 200 {
		t.Fatalf("create older entry: %d", st)
	}
	if st := post(staffCl, fmt.Sprintf(`{"version":"2.0.0-%s","release_date":"2026-06-01","category":"security","breaking":true,"title":"Newer %s","affected_endpoints":["/v1/api/campaigns"]}`, uniq, uniq)); st != 200 {
		t.Fatalf("create newer entry: %d", st)
	}

	// Public JSON feed (no auth): our two entries present, newest release first,
	// breaking flagged.
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
	var feed struct {
		Entries []struct {
			Version           string   `json:"version"`
			Category          string   `json:"category"`
			Breaking          bool     `json:"breaking"`
			Title             string   `json:"title"`
			AffectedEndpoints []string `json:"affected_endpoints"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(body, &feed); err != nil {
		t.Fatalf("feed decode: %v (%s)", err, body)
	}
	newerIdx, olderIdx, breakingOK := -1, -1, false
	for i, e := range feed.Entries {
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

	// Public HTML page renders the entry.
	pReq, _ := http.NewRequest(http.MethodGet, h.URLs.Gateway+routes.Changelog, nil)
	pResp, err := h.HTTP.Do(pReq)
	if err != nil {
		t.Fatalf("public page: %v", err)
	}
	pBody, _ := io.ReadAll(pResp.Body)
	pResp.Body.Close()
	if !strings.Contains(string(pBody), "Newer "+uniq) {
		t.Errorf("public /changelog page did not render the entry")
	}

	// Negative RBAC: an advertiser owner (no changelog:write) cannot publish.
	advCl := h.OwnerClient(t, w.AdvAcc.ID)
	if st := post(advCl, fmt.Sprintf(`{"version":"9.9.9-%s","release_date":"2026-01-01","title":"nope"}`, uniq)); st != http.StatusForbidden {
		t.Errorf("advertiser publish: got %d, want 403", st)
	}
}
