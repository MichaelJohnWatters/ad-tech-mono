package middleware

import (
	"net/http"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
)

func TestParseActAsTarget(t *testing.T) {
	cases := []struct {
		in       string
		wantType auth.AccountType
		wantID   string
	}{
		{"advertiser:acc-1", auth.AccountAdvertiser, "acc-1"},
		{"publisher:acc-2", auth.AccountPublisher, "acc-2"},
		{"acc-3", auth.AccountAdvertiser, "acc-3"},       // agency legacy (bare id)
		{"staff:acc-4", auth.AccountAdvertiser, "acc-4"}, // unrecognised type prefix → advertiser
		{"advertiser:", auth.AccountAdvertiser, ""},      // empty id
	}
	for _, c := range cases {
		gt, gid := ParseActAsTarget(c.in)
		if gt != c.wantType || gid != c.wantID {
			t.Errorf("ParseActAsTarget(%q) = (%s, %q), want (%s, %q)", c.in, gt, gid, c.wantType, c.wantID)
		}
	}
}

func TestActAsTarget_HeaderWinsThenCookie(t *testing.T) {
	// Header takes precedence.
	r, _ := http.NewRequest("GET", "/", nil)
	r.Header.Set(constants.HeaderActAs, "advertiser:from-header")
	r.AddCookie(&http.Cookie{Name: "act_as_account", Value: "advertiser:from-cookie"})
	if got := ActAsTarget(r); got != "advertiser:from-header" {
		t.Errorf("header should win: got %q", got)
	}

	// Cookie fallback when no header.
	r2, _ := http.NewRequest("GET", "/", nil)
	r2.AddCookie(&http.Cookie{Name: "act_as_account", Value: "publisher:from-cookie"})
	if got := ActAsTarget(r2); got != "publisher:from-cookie" {
		t.Errorf("cookie fallback: got %q", got)
	}

	// Neither → empty.
	r3, _ := http.NewRequest("GET", "/", nil)
	if got := ActAsTarget(r3); got != "" {
		t.Errorf("no act-as: got %q", got)
	}
}

// TestStripClientIdentityHeaders proves an inbound client cannot smuggle the
// gateway-trusted identity headers past the edge — the fix for the
// unauthenticated pass-through proxy laundering forged X-Account-Type: staff
// into a downstream cross-tenant read.
func TestStripClientIdentityHeaders(t *testing.T) {
	var seen http.Header
	h := StripClientIdentityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
	}))

	r, _ := http.NewRequest("GET", "/v1/reporting/trace?trace_id=x", nil)
	// A malicious client forging every trusted header.
	r.Header.Set(constants.HeaderAccountID, "victim-account")
	r.Header.Set(constants.HeaderAccountType, "staff")
	r.Header.Set(constants.HeaderUserID, "attacker")
	r.Header.Set(constants.HeaderPublisherID, "victim-pub")
	r.Header.Set(constants.HeaderActAs, "advertiser:victim") // must SURVIVE — see below
	r.Header.Set("X-Trace-ID", "keep-me")                    // a non-identity header must survive

	h.ServeHTTP(nil, r)

	for _, hdr := range trustedIdentityHeaders {
		if v := seen.Get(hdr); v != "" {
			t.Errorf("trusted header %q leaked through: %q", hdr, v)
		}
	}
	if seen.Get("X-Trace-ID") != "keep-me" {
		t.Errorf("non-identity header was stripped: %q", seen.Get("X-Trace-ID"))
	}
	// X-Act-As-Account must NOT be stripped at the edge: it's a client request
	// the gateway VALIDATES + resolves in ReverseProxy (then Del's on forward),
	// not a downstream-trusted injected header. Stripping it here silently broke
	// agency act-as (it no-op'd to the caller's own account). This asserts the
	// fix and guards against anyone re-adding it to trustedIdentityHeaders.
	if seen.Get(constants.HeaderActAs) != "advertiser:victim" {
		t.Errorf("X-Act-As-Account was stripped at the edge (%q) — act-as needs it to reach the gateway's resolver", seen.Get(constants.HeaderActAs))
	}
}
