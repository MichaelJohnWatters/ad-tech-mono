//go:build e2e

// Staff cross-user session revocation: a platform (staff) caller can kill ANOTHER
// user's sessions — the help-desk response for a user whose device was lost or
// stolen. A non-staff caller must NOT be able to target another user. Proven
// end-to-end: a victim's live token stops working after staff revokes it, while
// the staff caller's own token is untouched.
package e2e

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestStaffRevokesAnotherUsersSessions(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)

	// Victim advertiser with a live session.
	victim := h.Signup(t, "Victim Adv", fmt.Sprintf("victim-revoke-%d@api.test", time.Now().UnixNano()), "pw-e2e-1", "advertiser")
	gw, _ := url.Parse(h.URLs.Gateway)
	var vtoken string
	for _, c := range victim.Jar.Cookies(gw) {
		if c.Name == "adtech_session" {
			vtoken = c.Value
		}
	}
	if vtoken == "" {
		t.Fatal("no victim session cookie after signup")
	}
	// The API takes the victim's team-member id; derive it from the JWT subject.
	victimTeamID := auth.TeamMemberID(jwtSub(t, vtoken))
	if victimTeamID == "" {
		t.Fatal("could not derive victim team-member id from token")
	}

	staff := h.AdminToken(t) // platform caller (IsPlatformUser + support:update via "*")
	client := harness.NewHTTPClient(10 * time.Second)

	apiGet := func(tok string) int {
		req, _ := http.NewRequest(http.MethodGet, h.URLs.Gateway+routes.APINotifications, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	revoke := func(tok, body string) int {
		req, _ := http.NewRequest(http.MethodPost, h.URLs.Gateway+routes.AuthRevokeSessions, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("revoke: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// The token's iat must be strictly before the revoke cutoff second.
	time.Sleep(1100 * time.Millisecond)

	// Control: the victim's token works.
	if got := apiGet(vtoken); got != http.StatusOK {
		t.Fatalf("victim token before revoke = %d, want 200", got)
	}

	// A NON-staff caller (the victim) may not target another user → 403, and it
	// must be a no-op (the victim's own token still works afterwards).
	if got := revoke(vtoken, `{"user_id":"`+victimTeamID+`"}`); got != http.StatusForbidden {
		t.Errorf("non-staff cross-user revoke = %d, want 403", got)
	}
	if got := apiGet(vtoken); got != http.StatusOK {
		t.Fatalf("victim token after forbidden revoke = %d, want 200 (must be a no-op)", got)
	}

	// Staff revokes the victim's sessions.
	if got := revoke(staff, `{"user_id":"`+victimTeamID+`"}`); got != http.StatusOK {
		t.Fatalf("staff cross-user revoke = %d, want 200", got)
	}

	// The victim's token is now rejected; the staff caller's own token is not.
	if got := apiGet(vtoken); got != http.StatusUnauthorized {
		t.Errorf("victim token after staff revoke = %d, want 401", got)
	}
	if got := apiGet(staff); got != http.StatusOK {
		t.Errorf("staff's own token after revoking another user = %d, want 200 (must be unaffected)", got)
	}
}

// jwtSub extracts the "sub" (subject) claim from a JWT without verifying it —
// enough to learn the token's own user id in a test.
func jwtSub(t *testing.T, token string) string {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT: %d segments", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode jwt payload: %v", err)
	}
	var c struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("unmarshal jwt payload: %v", err)
	}
	return c.Sub
}
