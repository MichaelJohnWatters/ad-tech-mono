//go:build e2e

// External partner onboarding registry (PLAN Phase 11, item 112) — staff-facing
// slice 1. A staff owner registers a partner, walks it through the onboarding
// lifecycle (pending → sandbox → certified → active), and the state machine
// rejects illegal jumps (sandbox → active without certifying). Platform-global,
// staff-only (partners:read/manage).
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

// partnerReq does an authed JSON request and returns (status, decoded body).
func partnerReq(t *testing.T, client *http.Client, method, url, body string) (int, map[string]any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, _ := http.NewRequestWithContext(ctx, method, url, rdr)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return resp.StatusCode, out
}

func TestPartnerOnboardingLifecycle(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	staff := h.CreateStaff(t, fmt.Sprintf("partner-staff-%d", time.Now().UnixNano()))
	client := h.OwnerClient(t, staff.ID) // staff:owner → partners:read/manage

	partnersURL := h.URLs.Gateway + routes.APIPartners
	statusURL := h.URLs.Gateway + routes.APIPartnerStatus
	name := fmt.Sprintf("E2E DSP %d", time.Now().UnixNano())

	// 1) Register → 201, starts pending.
	code, p := partnerReq(t, client, http.MethodPost, partnersURL,
		fmt.Sprintf(`{"name":%q,"kind":"dsp","endpoint_bid":"https://bid.partner.example/openrtb","channels":["display","video"]}`, name))
	if code != http.StatusCreated {
		t.Fatalf("register partner = %d, want 201 (%v)", code, p)
	}
	id, _ := p["id"].(string)
	if id == "" || p["status"] != "pending" {
		t.Fatalf("registered partner malformed: %v", p)
	}

	// 2) It appears in the staff list.
	code, _ = partnerReq(t, client, http.MethodGet, partnersURL+"?status=pending", "")
	if code != http.StatusOK {
		t.Fatalf("list partners = %d, want 200", code)
	}

	// 3) Illegal jump pending → active (must go through sandbox+certified) → 409.
	code, _ = partnerReq(t, client, http.MethodPost, statusURL, fmt.Sprintf(`{"id":%q,"status":"active"}`, id))
	if code != http.StatusConflict {
		t.Errorf("pending→active = %d, want 409 (state machine)", code)
	}

	// 4) Legal path: pending → sandbox → certified → active.
	for _, st := range []string{"sandbox", "certified", "active"} {
		code, got := partnerReq(t, client, http.MethodPost, statusURL, fmt.Sprintf(`{"id":%q,"status":%q}`, id, st))
		if code != http.StatusOK || got["status"] != st {
			t.Fatalf("transition → %s = %d (%v), want 200 + status %s", st, code, got, st)
		}
	}

	// 5) Going active stamped onboarded_at (persisted).
	var onboarded *time.Time
	if err := h.DB.QueryRow(`SELECT onboarded_at FROM partners WHERE id = $1::uuid`, id).Scan(&onboarded); err != nil {
		t.Fatalf("read onboarded_at: %v", err)
	}
	if onboarded == nil {
		t.Error("onboarded_at not stamped after going active")
	}

	// 6) PARTIAL edit (only id+name+endpoint) must PRESERVE fields the body omits
	// (channels, openrtb_version) — not clobber them back to defaults.
	newName := name + " (edited)"
	code, p = partnerReq(t, client, http.MethodPost, partnersURL,
		fmt.Sprintf(`{"id":%q,"name":%q,"endpoint_bid":"https://bid2.partner.example"}`, id, newName))
	if code != http.StatusOK || p["name"] != newName {
		t.Fatalf("edit partner = %d name=%v, want 200 + %q", code, p["name"], newName)
	}
	if chans, _ := p["channels"].([]any); len(chans) != 2 {
		t.Errorf("partial edit clobbered channels: got %v, want the 2 registered", p["channels"])
	}
	if p["openrtb_version"] != "2.5" {
		t.Errorf("partial edit changed openrtb_version to %v (should be preserved)", p["openrtb_version"])
	}

	// 7) Client-error status codes (were 500s before the review): duplicate name
	// → 409, garbage id → 400, bad auth_method → 400.
	code, _ = partnerReq(t, client, http.MethodPost, partnersURL, fmt.Sprintf(`{"name":%q,"kind":"dsp"}`, newName))
	if code != http.StatusConflict {
		t.Errorf("duplicate-name register = %d, want 409", code)
	}
	code, _ = partnerReq(t, client, http.MethodGet, partnersURL+"?id=not-a-uuid", "")
	if code != http.StatusBadRequest {
		t.Errorf("garbage id GET = %d, want 400", code)
	}
	code, _ = partnerReq(t, client, http.MethodPost, partnersURL, `{"name":"bad-auth-partner","auth_method":"telepathy"}`)
	if code != http.StatusBadRequest {
		t.Errorf("bad auth_method = %d, want 400", code)
	}

	// 8) A non-staff account cannot read the registry (tenant/RBAC gate).
	adv := h.CreateAdvertiser(t, fmt.Sprintf("partner-adv-%d", time.Now().UnixNano()))
	advClient := h.OwnerClient(t, adv.ID)
	code, _ = partnerReq(t, advClient, http.MethodGet, partnersURL, "")
	if code != http.StatusForbidden {
		t.Errorf("advertiser GET partners = %d, want 403 (staff-only)", code)
	}
}
