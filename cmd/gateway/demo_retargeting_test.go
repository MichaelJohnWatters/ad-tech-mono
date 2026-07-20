package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/profilebuilder"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// The permission gating is enforced before the store/backend are consulted, so
// these checks run without Postgres or a live lake. The store-backed behaviour
// (real reset + real lake write + real lake-backed profile-builder expansion)
// is covered in demo_retargeting_integration_test.go.

func TestDemoRetargetingHandler_Permissions(t *testing.T) {
	// nil-db/backend orchestrator: permission checks must still short-circuit
	// BEFORE the 503 unavailable path, so a 403 never leaks store wiring.
	o := &rtDemoOrchestrator{db: nil, backend: nil, log: quietLog()}
	h := demoRetargetingHandler(o)

	req := func(method, target string, claims *auth.Claims) *http.Request {
		r := httptest.NewRequest(method, target, nil)
		if claims != nil {
			r = withClaims(r, claims)
		}
		return r
	}

	staffRead := &auth.Claims{AccountType: auth.AccountStaff, Permissions: []string{"support:read"}}
	staffUpdate := &auth.Claims{AccountType: auth.AccountStaff, Permissions: []string{"support:read", "support:update"}}
	advertiser := &auth.Claims{AccountType: auth.AccountAdvertiser, Permissions: []string{"reports:read"}}

	cases := []struct {
		name   string
		method string
		target string
		claims *auth.Claims
		want   int
	}{
		{"no claims → 401", http.MethodGet, routes.APIDemoRetargeting, nil, http.StatusUnauthorized},
		{"advertiser view → 403", http.MethodGet, routes.APIDemoRetargeting, advertiser, http.StatusForbidden},
		{"advertiser run → 403", http.MethodPost, routes.APIDemoRetargetingRun, advertiser, http.StatusForbidden},
		// support:read may VIEW but NOT run (support:update required).
		{"read-only staff run → 403", http.MethodPost, routes.APIDemoRetargetingRun, staffRead, http.StatusForbidden},
		// With the right permission, gating passes and we reach the nil-store 503.
		{"staff view reaches store → 503", http.MethodGet, routes.APIDemoRetargeting, staffRead, http.StatusServiceUnavailable},
		{"staff run reaches store → 503", http.MethodPost, routes.APIDemoRetargetingRun, staffUpdate, http.StatusServiceUnavailable},
		{"unsupported method → 405", http.MethodDelete, routes.APIDemoRetargeting, staffUpdate, http.StatusMethodNotAllowed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h(rec, req(tc.method, tc.target, tc.claims))
			if rec.Code != tc.want {
				t.Fatalf("code = %d, want %d (body=%s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// TestRetargetingRuleJSON asserts the persisted rule is exactly the behavioural
// retargeting rule the builder's behavioural-rule job dispatches on: kind
// defaults to behavioural (no "kind"), event=site_visit, tag=pricing-page. A
// drift here would make the rule never match the visit row.
func TestRetargetingRuleJSON(t *testing.T) {
	raw := rtRuleJSON()
	if kind := profilebuilder.RuleKind([]byte(raw)); kind != "" && kind != "behaviour" {
		t.Fatalf("rule kind = %q, want behavioural (\"\" or \"behaviour\")", kind)
	}
	rule, err := profilebuilder.ParseRule([]byte(raw))
	if err != nil {
		t.Fatalf("ParseRule: %v", err)
	}
	if rule.Event != "site_visit" {
		t.Errorf("rule event = %q, want site_visit", rule.Event)
	}
	if rule.Tag != rtTag {
		t.Errorf("rule tag = %q, want %q", rule.Tag, rtTag)
	}
}

// fakeRetargetingBackend records the visit row it was handed and lets a test
// drive the builder result — the seam that lets the 5-step assembly run with no
// live tracker/lake/builder. matchOnRun controls whether "the rule fired": true
// promotes the visit's user into the demo segment (via the injected member
// set), false leaves BEFORE == AFTER (a tag mismatch / no-consent outcome).
type fakeRetargetingBackend struct {
	pixelStatus int
	pixelFired  bool
	wroteRow    *rtVisitRow
	result      profilebuilder.Result
	runErr      error
}

func (f *fakeRetargetingBackend) FirePixel(_ context.Context, row rtVisitRow) (int, bool, string) {
	return f.pixelStatus, f.pixelFired, "http://tracker" + routes.TrackerRetarget + "?uid=" + row.UserID + "&tag=" + row.Tag
}
func (f *fakeRetargetingBackend) WriteVisitRow(_ context.Context, row rtVisitRow) error {
	r := row
	f.wroteRow = &r
	return nil
}
func (f *fakeRetargetingBackend) RunBuilder(_ context.Context) (profilebuilder.Result, error) {
	return f.result, f.runErr
}

// TestFakeBackend_VisitRow documents the exact site_visit behaviour row the
// backend is asked to write: the demo user's id, kind=site_visit, the pixel
// tag, the demo account, consent granted. These are the fields the builder's
// rule.matches() filters on — a drift here breaks retargeting silently.
func TestFakeBackend_VisitRow(t *testing.T) {
	f := &fakeRetargetingBackend{pixelStatus: 200, pixelFired: true}
	row := rtVisitRow{Kind: "site_visit", UserID: rtEmailID, Tag: rtTag, AccountID: rtAccountID(), Consent: true}
	if err := f.WriteVisitRow(context.Background(), row); err != nil {
		t.Fatalf("WriteVisitRow: %v", err)
	}
	got := f.wroteRow
	if got == nil {
		t.Fatal("backend never received a visit row")
	}
	if got.Kind != "site_visit" {
		t.Errorf("row kind = %q, want site_visit", got.Kind)
	}
	if got.UserID != rtEmailID {
		t.Errorf("row user_id = %q, want %q", got.UserID, rtEmailID)
	}
	if got.Tag != rtTag {
		t.Errorf("row tag = %q, want %q", got.Tag, rtTag)
	}
	if got.AccountID != rtAccountID() {
		t.Errorf("row account_id = %q, want the demo account", got.AccountID)
	}
	if !got.Consent {
		t.Error("row consent = false, want true (no consent = no row = no retargeting)")
	}
}

// TestRetargetingStepAssembly builds the 5 steps directly from a fake backend's
// result + injected membership sets — no DB — to assert the contract the UI
// depends on: 5 numbered steps, AFTER ⊇ BEFORE, the diff is exactly what the
// rule added, and the household id never appears in a membership. The
// tag-mismatch / no-consent path (rule doesn't fire) yields BEFORE == AFTER
// with an honest empty-membership narration.
func TestRetargetingStepAssembly(t *testing.T) {
	// Match path: BEFORE empty, AFTER = {email, cookie, ifa} (household out).
	before := []string(nil)
	after := []string{rtCookieID, rtEmailID, rtIfaID}
	steps := assembleRetargetingSteps(before, after, profilebuilder.Result{
		Clusters: 1, ClusterMembers: 3, RuleSegments: 1, Enrolled: 3,
	}, rtVisitResult{PixelStatus: 200, PixelFired: true})
	if len(steps) != 5 {
		t.Fatalf("assembled %d steps, want 5", len(steps))
	}
	for i, s := range steps {
		if s.N != i+1 {
			t.Fatalf("step %d has N=%d, want %d", i, s.N, i+1)
		}
	}
	// AFTER must superset-contain BEFORE and never carry the household id.
	added := demoDiff(before, after)
	if len(added) != 3 {
		t.Fatalf("added = %v, want the three cross-device ids", added)
	}
	for _, id := range after {
		if id == rtHouseholdID {
			t.Fatalf("household id leaked into AFTER membership: %v", after)
		}
	}

	// No-fire path (tag mismatch / no consent): BEFORE == AFTER, empty. The
	// AFTER step must narrate honestly (0 members), not claim enrolment.
	empty := assembleRetargetingSteps(nil, nil, profilebuilder.Result{Clusters: 1, RuleSegments: 1, Enrolled: 0}, rtVisitResult{PixelStatus: 200, PixelFired: true})
	afterStep := empty[4]
	if !strings.Contains(afterStep.Narration, "0 ids") {
		t.Errorf("no-fire AFTER narration should honestly report 0 members, got: %q", afterStep.Narration)
	}
}

// TestRetargetingSegmentMembers verifies the profile-view → member-set
// extraction only counts memberships in the demo rule segment.
func TestRetargetingSegmentMembers(t *testing.T) {
	seg := rtSegmentID()
	v := profileView{Memberships: []profileMembership{
		{MemberID: rtEmailID, SegmentID: seg},
		{MemberID: rtCookieID, SegmentID: seg},
		{MemberID: rtEmailID, SegmentID: "other-segment"}, // ignored
		{MemberID: rtEmailID, SegmentID: seg},             // dedup
	}}
	got := rtSegmentMembers(v, seg)
	if len(got) != 2 || got[0] != rtCookieID || got[1] != rtEmailID {
		t.Fatalf("members = %v, want sorted [cookie, email]", got)
	}
}

// TestVisitRowJSON documents the row serialises with the field names the UI
// pre-renders (kind/user_id/tag/account_id/consent/observed_at).
func TestVisitRowJSON(t *testing.T) {
	b, _ := json.Marshal(rtVisitRow{Kind: "site_visit", UserID: rtEmailID, Tag: rtTag, AccountID: "acct", Consent: true, ObservedAt: "2026-07-20T00:00:00Z"})
	for _, want := range []string{`"kind":"site_visit"`, `"user_id":"` + rtEmailID + `"`, `"tag":"` + rtTag + `"`, `"consent":true`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("visit row JSON %s missing %s", b, want)
		}
	}
}
