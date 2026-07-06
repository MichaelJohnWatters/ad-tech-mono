//go:build e2e

// Agency act-as / managed accounts. Staff assign advertiser accounts to an
// agency; the agency's session loads them into its claims at login; and the
// gateway's act-as forwards a selected managed account as the effective tenant
// (validated against the managed set) so the agency operates as that advertiser.
package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	neturl "net/url"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestAgencyActAsViaAPI(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.Reset(t)

	uniq := fmt.Sprintf("agy-%d", time.Now().UnixNano())

	// Agency account + an agency:owner login.
	agency := h.CreateAgency(t, uniq+"-agency")
	agencyEmail := uniq + "-agency@login.test"
	h.CreateLoginUser(t, agency.ID, agencyEmail, "pw-e2e-1", "owner")

	// Two advertiser accounts: A is managed by the agency, B is not.
	managed := h.CreateAdvertiser(t, uniq+"-managed")
	other := h.CreateAdvertiser(t, uniq+"-other")

	// Staff assigns A to the agency (mint an admin — Reset wiped the seed).
	admin := h.CreateAdmin(t, uniq+"-admin")
	adminEmail := uniq + "-admin@login.test"
	h.CreateLoginUser(t, admin.ID, adminEmail, "pw-e2e-1", "owner")
	staff := h.LoginAs(t, adminEmail, "pw-e2e-1")
	if code := h.APIStatus(t, staff, http.MethodPost, "/v1/api/agency-accounts",
		fmt.Sprintf(`{"agency_account_id":%q,"managed_account_id":%q}`, agency.ID, managed.ID)); code != http.StatusCreated {
		t.Fatalf("staff assign managed account = %d, want 201", code)
	}
	// Assigning a non-advertiser (the agency itself) is rejected.
	if code := h.APIStatus(t, staff, http.MethodPost, "/v1/api/agency-accounts",
		fmt.Sprintf(`{"agency_account_id":%q,"managed_account_id":%q}`, agency.ID, agency.ID)); code != http.StatusBadRequest {
		t.Errorf("assign non-advertiser = %d, want 400", code)
	}

	// Agency logs in AFTER assignment so its claims carry the managed set.
	ag := h.LoginAs(t, agencyEmail, "pw-e2e-1")

	// The agency lists its own managed accounts → sees A only.
	req, _ := http.NewRequest(http.MethodGet, h.URLs.Gateway+"/v1/api/agency-accounts", nil)
	resp, _ := ag.Do(req)
	var links []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&links)
	resp.Body.Close()
	if len(links) != 1 || links[0]["managed_account_id"] != managed.ID {
		t.Fatalf("agency managed list = %+v, want just %s", links, managed.ID)
	}

	// Act-as helper: POST a campaign with the given act-as target.
	actAsCreate := func(actAs, name string) (int, string) {
		body := fmt.Sprintf(`{"name":%q,"base_bid":3.0,"daily_budget":500}`, name)
		req, _ := http.NewRequest(http.MethodPost, h.URLs.Gateway+"/v1/api/campaigns", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if actAs != "" {
			req.Header.Set("X-Act-As-Account", actAs)
		}
		resp, err := ag.Do(req)
		if err != nil {
			t.Fatalf("act-as create: %v", err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		acct, _ := out["account_id"].(string)
		return resp.StatusCode, acct
	}

	// Act-as the managed advertiser → the campaign lands under A's account.
	// (The proxied DSP create returns 200, not 201 — accept any 2xx.)
	code, acct := actAsCreate(managed.ID, "Agency Campaign")
	if code < 200 || code >= 300 {
		t.Fatalf("act-as managed create = %d, want 2xx", code)
	}
	if acct != managed.ID {
		t.Errorf("campaign landed under %s, want the managed advertiser %s", acct, managed.ID)
	}

	// Act-as a NON-managed advertiser → the gateway rejects it.
	if code, _ := actAsCreate(other.ID, "Should Fail"); code != http.StatusForbidden {
		t.Errorf("act-as non-managed = %d, want 403", code)
	}

	// With act-as A, the agency sees A's campaigns; without act-as it sees its
	// own (empty) account — the campaign is not visible.
	listCampaigns := func(actAs string) int {
		req, _ := http.NewRequest(http.MethodGet, h.URLs.Gateway+"/v1/api/campaigns", nil)
		if actAs != "" {
			req.Header.Set("X-Act-As-Account", actAs)
		}
		resp, err := ag.Do(req)
		if err != nil {
			t.Fatalf("list campaigns: %v", err)
		}
		defer resp.Body.Close()
		var list []map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&list)
		return len(list)
	}
	// Read-after-write: the campaign list can lag the create briefly (warm
	// cache / eventual read), so poll rather than assert once.
	harness.WaitFor(t, 10*time.Second, "act-as A sees its 1 campaign", func() bool {
		return listCampaigns(managed.ID) == 1
	})
	if n := listCampaigns(""); n != 0 {
		t.Errorf("no-act-as campaign count = %d, want 0 (agency's own account is empty)", n)
	}

	// Portfolio data path: the agency roll-up runs a report per managed account
	// via act-as. A managed target is scoped + allowed (200); a non-managed one
	// is rejected by the proxy (403).
	reportAs := func(actAs string) int {
		req, _ := http.NewRequest(http.MethodPost, h.URLs.Gateway+"/v1/api/reports",
			strings.NewReader(`{"table":"impressions","metrics":["count"]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Act-As-Account", actAs)
		resp, err := ag.Do(req)
		if err != nil {
			t.Fatalf("report act-as: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := reportAs(managed.ID); code != http.StatusOK {
		t.Errorf("report as managed account = %d, want 200", code)
	}
	if code := reportAs(other.ID); code != http.StatusForbidden {
		t.Errorf("report as non-managed account = %d, want 403", code)
	}

	// The portal switcher acts-as via an act_as_account COOKIE (not a header) —
	// verify the proxy honours that path too. Setting the cookie on the jar
	// makes a header-less GET scope to the managed account.
	gwURL, _ := neturl.Parse(h.URLs.Gateway)
	ag.Jar.SetCookies(gwURL, []*http.Cookie{{Name: "act_as_account", Value: managed.ID}})
	if n := listCampaigns(""); n != 1 {
		t.Errorf("cookie-based act-as campaign count = %d, want 1", n)
	}
}
