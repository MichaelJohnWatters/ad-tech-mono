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

// TestPartnerProvisionAndSelfServe (slice 2): staff registers a partner and
// provisions its self-serve login; the partner logs in and reads its own
// onboarding record via /v1/api/partner/me, but cannot reach the staff registry.
func TestPartnerProvisionAndSelfServe(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	staff := h.CreateStaff(t, fmt.Sprintf("prov-staff-%d", time.Now().UnixNano()))
	client := h.OwnerClient(t, staff.ID)

	partnersURL := h.URLs.Gateway + routes.APIPartners
	name := fmt.Sprintf("E2E SSP %d", time.Now().UnixNano())

	// Register a partner.
	code, p := partnerReq(t, client, http.MethodPost, partnersURL,
		fmt.Sprintf(`{"name":%q,"kind":"ssp","endpoint_bid":"https://bid.ssp.example"}`, name))
	if code != http.StatusCreated {
		t.Fatalf("register = %d, want 201", code)
	}
	id, _ := p["id"].(string)

	// Provision a login for it → 201 + one-time temp password.
	email := fmt.Sprintf("partner-%d@integrations.test", time.Now().UnixNano())
	code, prov := partnerReq(t, client, http.MethodPost, h.URLs.Gateway+routes.APIPartnerProvision,
		fmt.Sprintf(`{"id":%q,"email":%q}`, id, email))
	if code != http.StatusCreated {
		t.Fatalf("provision = %d, want 201 (%v)", code, prov)
	}
	temp, _ := prov["temp_password"].(string)
	if temp == "" || prov["account_id"] == "" {
		t.Fatalf("provision response missing temp_password/account_id: %v", prov)
	}

	// Double-provision → 409.
	code, _ = partnerReq(t, client, http.MethodPost, h.URLs.Gateway+routes.APIPartnerProvision,
		fmt.Sprintf(`{"id":%q,"email":"other-%d@integrations.test"}`, id, time.Now().UnixNano()))
	if code != http.StatusConflict {
		t.Errorf("double provision = %d, want 409", code)
	}

	// The partner logs in with the temp password and reads its own record.
	partnerClient := h.LoginAs(t, email, temp)
	code, me := partnerReq(t, partnerClient, http.MethodGet, h.URLs.Gateway+routes.APIPartnerMe, "")
	if code != http.StatusOK {
		t.Fatalf("partner /me = %d, want 200 (%v)", code, me)
	}
	if me["name"] != name || me["status"] != "pending" {
		t.Errorf("partner /me = %v, want name %q status pending", me, name)
	}

	// The partner CANNOT read the staff registry.
	code, _ = partnerReq(t, partnerClient, http.MethodGet, partnersURL, "")
	if code != http.StatusForbidden {
		t.Errorf("partner GET staff registry = %d, want 403", code)
	}

	// A second partner. Provisioning it with partner 1's email → 409 (email in use).
	name2 := name + " two"
	_, p2 := partnerReq(t, client, http.MethodPost, partnersURL, fmt.Sprintf(`{"name":%q,"kind":"dsp"}`, name2))
	id2, _ := p2["id"].(string)
	code, _ = partnerReq(t, client, http.MethodPost, h.URLs.Gateway+routes.APIPartnerProvision,
		fmt.Sprintf(`{"id":%q,"email":%q}`, id2, email))
	if code != http.StatusConflict {
		t.Errorf("email-collision provision = %d, want 409", code)
	}
	// Provision partner 2 with its own email → its /me returns ITS record, proving
	// per-account isolation (not partner 1's).
	email2 := fmt.Sprintf("partner2-%d@integrations.test", time.Now().UnixNano())
	code, prov2 := partnerReq(t, client, http.MethodPost, h.URLs.Gateway+routes.APIPartnerProvision,
		fmt.Sprintf(`{"id":%q,"email":%q}`, id2, email2))
	if code != http.StatusCreated {
		t.Fatalf("provision p2 = %d", code)
	}
	temp2, _ := prov2["temp_password"].(string)
	client2 := h.LoginAs(t, email2, temp2)
	_, me2 := partnerReq(t, client2, http.MethodGet, h.URLs.Gateway+routes.APIPartnerMe, "")
	if me2["name"] != name2 {
		t.Errorf("partner2 /me = %v, want its own record %q (cross-partner isolation)", me2["name"], name2)
	}
}

// partnerList does an authed GET returning a JSON array.
func partnerListReq(t *testing.T, client *http.Client, url string) (int, []map[string]any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	var out []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// TestPartnerSandboxKeys (slice 2b): a provisioned partner self-serves its
// sandbox API key — generate (full value once), list (masked), rotate (grace
// window), revoke. Strictly account-scoped; non-partners are forbidden.
func TestPartnerSandboxKeys(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	staff := h.CreateStaff(t, fmt.Sprintf("key-staff-%d", time.Now().UnixNano()))
	client := h.OwnerClient(t, staff.ID)

	// Register + provision a partner login.
	_, p := partnerReq(t, client, http.MethodPost, h.URLs.Gateway+routes.APIPartners,
		fmt.Sprintf(`{"name":"E2E KeyPartner %d","kind":"dsp"}`, time.Now().UnixNano()))
	id, _ := p["id"].(string)
	email := fmt.Sprintf("keypartner-%d@integrations.test", time.Now().UnixNano())
	_, prov := partnerReq(t, client, http.MethodPost, h.URLs.Gateway+routes.APIPartnerProvision,
		fmt.Sprintf(`{"id":%q,"email":%q}`, id, email))
	temp, _ := prov["temp_password"].(string)
	partnerClient := h.LoginAs(t, email, temp)

	keysURL := h.URLs.Gateway + routes.APIPartnerSandboxKeys
	revokeURL := h.URLs.Gateway + routes.APIPartnerSandboxKeysRevoke

	// Starts empty.
	code, list := partnerListReq(t, partnerClient, keysURL)
	if code != http.StatusOK || len(list) != 0 {
		t.Fatalf("initial keys = %d len %d, want 200 empty", code, len(list))
	}

	// Generate → 201 + full value once (sk_ prefix).
	code, gen := partnerReq(t, partnerClient, http.MethodPost, keysURL, "")
	if code != http.StatusCreated {
		t.Fatalf("generate key = %d, want 201 (%v)", code, gen)
	}
	full, _ := gen["value"].(string)
	firstID, _ := gen["id"].(string)
	if !strings.HasPrefix(full, "sk_") || firstID == "" {
		t.Fatalf("generated key malformed: %v", gen)
	}

	// List shows it ACTIVE + MASKED (never the full value).
	code, list = partnerListReq(t, partnerClient, keysURL)
	if code != http.StatusOK || len(list) != 1 || list[0]["status"] != "active" {
		t.Fatalf("after generate: %d %v", code, list)
	}
	if prev, _ := list[0]["value_preview"].(string); prev == "" || prev == full {
		t.Errorf("value_preview = %q must be masked, not the full key", list[0]["value_preview"])
	}

	// Rotate → the old key becomes 'rotating', the new one 'active' (2 total).
	code, _ = partnerReq(t, partnerClient, http.MethodPost, keysURL, "")
	if code != http.StatusCreated {
		t.Fatalf("rotate = %d, want 201", code)
	}
	code, list = partnerListReq(t, partnerClient, keysURL)
	if code != http.StatusOK || len(list) != 2 {
		t.Fatalf("after rotate: %d len %d, want 2", code, len(list))
	}

	// Revoke the first key → it drops out of the list.
	code, _ = partnerReq(t, partnerClient, http.MethodPost, revokeURL, fmt.Sprintf(`{"id":%q}`, firstID))
	if code != http.StatusOK {
		t.Errorf("revoke = %d, want 200", code)
	}
	code, list = partnerListReq(t, partnerClient, keysURL)
	if code != http.StatusOK || len(list) != 1 {
		t.Errorf("after revoke: len %d, want 1", len(list))
	}

	// A staff (non-partner) account cannot touch partner sandbox keys.
	code, _ = partnerListReq(t, client, keysURL)
	if code != http.StatusForbidden {
		t.Errorf("staff GET sandbox-keys = %d, want 403 (partner-only)", code)
	}
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
