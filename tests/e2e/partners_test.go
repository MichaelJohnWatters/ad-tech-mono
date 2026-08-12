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
		t.Fatalf("after revoke: len %d, want 1", len(list))
	}
	survivingKeyID, _ := list[0]["id"].(string)

	// Cross-partner isolation: a SECOND partner can't see partner 1's keys, and
	// can't revoke partner 1's key by id (scoped to account_id → 404).
	_, p2 := partnerReq(t, client, http.MethodPost, h.URLs.Gateway+routes.APIPartners,
		fmt.Sprintf(`{"name":"E2E KeyPartner2 %d","kind":"dsp"}`, time.Now().UnixNano()))
	id2, _ := p2["id"].(string)
	email2 := fmt.Sprintf("keypartner2-%d@integrations.test", time.Now().UnixNano())
	_, prov2 := partnerReq(t, client, http.MethodPost, h.URLs.Gateway+routes.APIPartnerProvision,
		fmt.Sprintf(`{"id":%q,"email":%q}`, id2, email2))
	temp2, _ := prov2["temp_password"].(string)
	client2 := h.LoginAs(t, email2, temp2)

	code, list2 := partnerListReq(t, client2, keysURL)
	if code != http.StatusOK || len(list2) != 0 {
		t.Errorf("partner2 sees %d keys, want 0 (isolation)", len(list2))
	}
	code, _ = partnerReq(t, client2, http.MethodPost, revokeURL, fmt.Sprintf(`{"id":%q}`, survivingKeyID))
	if code != http.StatusNotFound {
		t.Errorf("partner2 revoking partner1's key = %d, want 404 (account-scoped)", code)
	}

	// A staff (non-partner) account cannot touch partner sandbox keys.
	code, _ = partnerListReq(t, client, keysURL)
	if code != http.StatusForbidden {
		t.Errorf("staff GET sandbox-keys = %d, want 403 (partner-only)", code)
	}
}

// TestPartnerConformanceTools (slice 3): the partner self-serves the OpenRTB
// conformance validator (paste a response) and a live test-bid against its
// registered endpoint (graceful on unreachable).
func TestPartnerConformanceTools(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	staff := h.CreateStaff(t, fmt.Sprintf("conf-staff-%d", time.Now().UnixNano()))
	client := h.OwnerClient(t, staff.ID)

	// Register a partner with an UNREACHABLE bid endpoint (closed port) + provision.
	_, p := partnerReq(t, client, http.MethodPost, h.URLs.Gateway+routes.APIPartners,
		fmt.Sprintf(`{"name":"E2E ConfPartner %d","kind":"dsp","endpoint_bid":"http://dsp-internal:59999/bid"}`, time.Now().UnixNano()))
	id, _ := p["id"].(string)
	email := fmt.Sprintf("conf-%d@integrations.test", time.Now().UnixNano())
	_, prov := partnerReq(t, client, http.MethodPost, h.URLs.Gateway+routes.APIPartnerProvision,
		fmt.Sprintf(`{"id":%q,"email":%q}`, id, email))
	temp, _ := prov["temp_password"].(string)
	pc := h.LoginAs(t, email, temp)

	validateURL := h.URLs.Gateway + routes.APIPartnerValidate

	// A conformant response → valid, no error findings.
	good := `{"id":"validate-probe","cur":"USD","seatbid":[{"seat":"you","bid":[{"id":"b1","impid":"1","price":1.25,"adm":"<div>ad</div>","crid":"cr1","adomain":["you.example"]}]}]}`
	code, res := partnerReq(t, pc, http.MethodPost, validateURL, good)
	if code != http.StatusOK || res["valid"] != true {
		t.Fatalf("validate good = %d valid=%v, want 200 valid (%v)", code, res["valid"], res)
	}

	// A non-conformant response (price 0, below floor, no creative) → not valid.
	bad := `{"id":"validate-probe","seatbid":[{"seat":"you","bid":[{"impid":"1","price":0}]}]}`
	code, res = partnerReq(t, pc, http.MethodPost, validateURL, bad)
	if code != http.StatusOK || res["valid"] != false {
		t.Errorf("validate bad = %d valid=%v, want 200 not-valid", code, res["valid"])
	}
	if fs, _ := res["findings"].([]any); len(fs) == 0 {
		t.Error("expected conformance findings for the bad response")
	}

	// A no-bid is valid.
	code, res = partnerReq(t, pc, http.MethodPost, validateURL, `{"id":"validate-probe","nobid":true}`)
	if res["valid"] != true {
		t.Errorf("no-bid validate valid=%v, want true", res["valid"])
	}

	// Live test-bid against the unreachable endpoint → gracefully not-valid,
	// never a 500.
	code, res = partnerReq(t, pc, http.MethodPost, h.URLs.Gateway+routes.APIPartnerTestBid, "")
	if code != http.StatusOK {
		t.Fatalf("test-bid = %d, want 200 (graceful) (%v)", code, res)
	}
	if res["valid"] != false {
		t.Errorf("test-bid against unreachable endpoint valid=%v, want false", res["valid"])
	}
	// The SSRF guard must have refused the private ClusterIP (not merely timed out).
	if fs, _ := res["findings"].([]any); len(fs) > 0 {
		if f, _ := fs[0].(map[string]any); f != nil {
			if msg, _ := f["message"].(string); !strings.Contains(msg, "non-public") {
				t.Errorf("test-bid finding = %q, want the SSRF guard to refuse the non-public address", msg)
			}
		}
	}

	// A non-partner (staff) cannot use these tools.
	code, _ = partnerReq(t, client, http.MethodPost, validateURL, good)
	if code != http.StatusForbidden {
		t.Errorf("staff validate = %d, want 403 (partner-only)", code)
	}
}

// TestPartnerCertification (slice 4): a sandbox partner submits conformant
// responses to the golden scenarios → passes → auto-advances to certified. A
// non-conformant submission fails and leaves the partner in sandbox.
func TestPartnerCertification(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	staff := h.CreateStaff(t, fmt.Sprintf("cert-staff-%d", time.Now().UnixNano()))
	client := h.OwnerClient(t, staff.ID)
	statusURL := h.URLs.Gateway + routes.APIPartnerStatus
	certifyURL := h.URLs.Gateway + routes.APIPartnerCertify

	// A helper: register + provision + login a partner already advanced to sandbox.
	sandboxPartner := func(tag string) (*http.Client, string) {
		_, p := partnerReq(t, client, http.MethodPost, h.URLs.Gateway+routes.APIPartners,
			fmt.Sprintf(`{"name":"E2E Cert %s %d","kind":"dsp"}`, tag, time.Now().UnixNano()))
		id, _ := p["id"].(string)
		email := fmt.Sprintf("cert-%s-%d@integrations.test", tag, time.Now().UnixNano())
		_, prov := partnerReq(t, client, http.MethodPost, h.URLs.Gateway+routes.APIPartnerProvision,
			fmt.Sprintf(`{"id":%q,"email":%q}`, id, email))
		temp, _ := prov["temp_password"].(string)
		if code, _ := partnerReq(t, client, http.MethodPost, statusURL, fmt.Sprintf(`{"id":%q,"status":"sandbox"}`, id)); code != http.StatusOK {
			t.Fatalf("advance to sandbox = %d", code)
		}
		return h.LoginAs(t, email, temp), id
	}

	// --- PASS: conformant responses (one real bid + two no-bids) → certified. ---
	pc, _ := sandboxPartner("pass")

	// GET returns the golden scenarios + current status.
	code, got := partnerReq(t, pc, http.MethodGet, certifyURL, "")
	if code != http.StatusOK || got["status"] != "sandbox" {
		t.Fatalf("GET certify = %d status=%v, want 200 sandbox", code, got["status"])
	}
	if scen, _ := got["scenarios"].([]any); len(scen) < 3 {
		t.Fatalf("expected >=3 golden scenarios, got %v", got["scenarios"])
	}

	good := `{"responses":{
		"standard":{"id":"cert-standard","cur":"USD","seatbid":[{"seat":"you","bid":[{"id":"b1","impid":"1","price":1.0,"adm":"<div>ad</div>","crid":"cr1","adomain":["you.example"]}]}]},
		"respect_floor":{"id":"cert-respect-floor","nobid":true},
		"honour_badv":{"id":"cert-honour-badv","nobid":true}
	}}`
	code, res := partnerReq(t, pc, http.MethodPost, certifyURL, good)
	if code != http.StatusOK {
		t.Fatalf("certify POST = %d (%v)", code, res)
	}
	if res["promoted"] != true || res["status"] != "certified" {
		t.Errorf("pass run promoted=%v status=%v, want promoted true + certified (%v)", res["promoted"], res["status"], res)
	}
	// The partner's own record now shows certified.
	_, me := partnerReq(t, pc, http.MethodGet, h.URLs.Gateway+routes.APIPartnerMe, "")
	if me["status"] != "certified" {
		t.Errorf("partner /me status = %v, want certified", me["status"])
	}
	// The run is in history.
	_, hist := partnerReq(t, pc, http.MethodGet, certifyURL, "")
	if hs, _ := hist["history"].([]any); len(hs) == 0 {
		t.Error("certification run not recorded in history")
	}

	// --- FAIL: a non-conformant response (price 0, no creative) → stays sandbox. ---
	fc, _ := sandboxPartner("fail")
	bad := `{"responses":{"standard":{"seatbid":[{"bid":[{"impid":"1","price":0}]}]},"respect_floor":{"nobid":true},"honour_badv":{"nobid":true}}}`
	code, res = partnerReq(t, fc, http.MethodPost, certifyURL, bad)
	if code != http.StatusOK {
		t.Fatalf("fail certify = %d", code)
	}
	if res["promoted"] != false || res["status"] != "sandbox" {
		t.Errorf("failed run promoted=%v status=%v, want not-promoted + sandbox", res["promoted"], res["status"])
	}

	// A non-partner (staff) can't run certification.
	code, _ = partnerReq(t, client, http.MethodGet, certifyURL, "")
	if code != http.StatusForbidden {
		t.Errorf("staff GET certify = %d, want 403", code)
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
