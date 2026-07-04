//go:build e2e

// Campaign depth via the real API: the create/edit endpoints now expose
// bid_strategy, pacing_mode, total_budget and flight dates (previously
// hardcoded cpm/asap). This proves an advertiser can self-serve a non-CPM
// campaign — the billing engine + money loop already support CPC/CPA/vCPM,
// they were just unreachable through the API.
package e2e

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestCampaignDepthViaAPI(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.Reset(t)

	uniq := fmt.Sprintf("depth-%d", time.Now().UnixNano())
	adv := h.Signup(t, "Depth Adv", uniq+"@api.test", "pw-e2e-1", "advertiser")

	// Create a CPC, front-loaded campaign with an explicit total budget and
	// flight window — all fields the API used to ignore.
	created := h.APIJSON(t, adv, http.MethodPost, "/v1/api/campaigns", `{
		"name":"Depth CPC",
		"base_bid":1.75,
		"daily_budget":200,
		"bid_strategy":"cpc",
		"pacing_mode":"front_loaded",
		"total_budget":4000,
		"start_date":"2026-07-01",
		"end_date":"2026-09-30"
	}`)
	campaignID, _ := created["id"].(string)
	if campaignID == "" {
		t.Fatalf("create returned no id: %v", created)
	}
	h.RefreshAllCaches(t)

	// Read it back through the API — the warm cache + GET expose BidModel /
	// PacingMode, so this proves the fields persisted through the whole path.
	req, _ := http.NewRequest(http.MethodGet, h.URLs.Gateway+"/v1/api/campaigns", nil)
	resp, err := adv.Do(req)
	if err != nil {
		t.Fatalf("campaign list: %v", err)
	}
	var list []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	var got map[string]any
	for _, c := range list {
		if c["ID"] == campaignID {
			got = c
			break
		}
	}
	if got == nil {
		t.Fatalf("created campaign %s not in list %v", campaignID, list)
	}
	if got["BidModel"] != "cpc" {
		t.Errorf("BidModel = %v, want cpc", got["BidModel"])
	}
	if got["PacingMode"] != "front_loaded" {
		t.Errorf("PacingMode = %v, want front_loaded", got["PacingMode"])
	}
	if got["TotalBudget"].(float64) != 4000 {
		t.Errorf("TotalBudget = %v, want 4000", got["TotalBudget"])
	}

	// The IO carries the flight window we asked for (not the 90-day default).
	advAccountID, _ := got["AccountID"].(string)
	h.WithTenant(t, advAccountID, func(tx *sql.Tx) {
		var start, end string
		if err := tx.QueryRow(
			`SELECT io.start_date::text, io.end_date::text
			 FROM insertion_orders io
			 JOIN line_items li ON li.insertion_order_id = io.id
			 WHERE li.id = $1::uuid`, campaignID).Scan(&start, &end); err != nil {
			t.Fatalf("io dates: %v", err)
		}
		if start != "2026-07-01" || end != "2026-09-30" {
			t.Errorf("flight window = %s..%s, want 2026-07-01..2026-09-30", start, end)
		}
	})

	// Edit flips the bid model to CPA and pacing to even — the PATCH depth.
	h.APIJSON(t, adv, http.MethodPatch, "/v1/api/campaigns/"+campaignID,
		`{"bid_strategy":"cpa","pacing_mode":"even"}`)
	h.RefreshAllCaches(t)
	req, _ = http.NewRequest(http.MethodGet, h.URLs.Gateway+"/v1/api/campaigns", nil)
	resp, _ = adv.Do(req)
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	for _, c := range list {
		if c["ID"] == campaignID {
			if c["BidModel"] != "cpa" || c["PacingMode"] != "even" {
				t.Errorf("after edit: BidModel=%v PacingMode=%v, want cpa/even", c["BidModel"], c["PacingMode"])
			}
		}
	}

	// Bad values are rejected with 400, not a raw Postgres constraint error.
	req, _ = http.NewRequest(http.MethodPost, h.URLs.Gateway+"/v1/api/campaigns",
		strings.NewReader(`{"name":"bad","bid_strategy":"cpx"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err = adv.Do(req)
	if err != nil {
		t.Fatalf("bad create: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid bid_strategy = %d, want 400", resp.StatusCode)
	}
}
