//go:build e2e

// Campaign format: advertisers can create non-display (video/native/audio)
// campaigns, not just display. A display campaign still gets an auto-generated
// placeholder banner; a non-display campaign starts creative-less (attach a
// real, format-matching creative via the creatives PATCH).
package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestCampaignFormatViaAPI(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.Reset(t)

	uniq := fmt.Sprintf("fmt-%d", time.Now().UnixNano())
	adv := h.Signup(t, "Fmt Adv", uniq+"-adv@api.test", "pw-e2e-1", "advertiser")

	created := h.APIJSON(t, adv, http.MethodPost, "/v1/api/campaigns",
		`{"name":"Video Campaign","base_bid":3.0,"daily_budget":500,"format":"video"}`)
	videoID := created["id"].(string)

	dispCreated := h.APIJSON(t, adv, http.MethodPost, "/v1/api/campaigns",
		`{"name":"Display Campaign","base_bid":3.0,"daily_budget":500}`) // default format
	displayID := dispCreated["id"].(string)

	// Read both back; check format + creative-count expectations.
	req, _ := http.NewRequest(http.MethodGet, h.URLs.Gateway+"/v1/api/campaigns", nil)
	resp, _ := adv.Do(req)
	var list []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()

	seen := map[string]map[string]any{}
	for _, c := range list {
		seen[c["ID"].(string)] = c
	}
	v, d := seen[videoID], seen[displayID]
	if v == nil || d == nil {
		t.Fatalf("campaigns not both in list (video=%v display=%v)", v != nil, d != nil)
	}
	if v["Format"] != "video" {
		t.Errorf("video campaign Format = %v, want video", v["Format"])
	}
	if d["Format"] != "display" {
		t.Errorf("display campaign Format = %v, want display", d["Format"])
	}
	// Display auto-generates a creative; video starts creative-less.
	if creatives, _ := v["Creatives"].([]any); len(creatives) != 0 {
		t.Errorf("video campaign has %d creatives, want 0 (creative-less until attached)", len(creatives))
	}
	if creatives, _ := d["Creatives"].([]any); len(creatives) != 1 {
		t.Errorf("display campaign has %d creatives, want 1 (auto-generated banner)", len(creatives))
	}

	// A bad format is rejected.
	if code := h.APIStatus(t, adv, http.MethodPost, "/v1/api/campaigns",
		`{"name":"Bad","format":"hologram"}`); code != http.StatusBadRequest {
		t.Errorf("bad format = %d, want 400", code)
	}
}
