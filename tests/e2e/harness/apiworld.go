//go:build e2e

package harness

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"
)

// APIWorld is a world built entirely through the real customer APIs — the
// self-testing counterpart to BuildBasicWorld's direct-SQL fixture. Every
// field came out of an API response; nothing was written to the database
// directly, so building one exercises signup, sessions, tenant scoping,
// the management CRUD paths, and the cache-invalidation fan-out.
type APIWorld struct {
	// Advertiser and Publisher are logged-in tenant sessions (cookie-jar
	// clients) for the two accounts the journey created.
	Advertiser *http.Client
	Publisher  *http.Client

	PublisherID string // publishers row (site) created via POST /v1/api/publishers
	PlacementID string // raw UUID — the SSP accepts it directly on ad requests
	CampaignID  string // line item created via POST /v1/api/campaigns
}

// Signup registers a new self-serve account through the real signup flow and
// returns a logged-in session (signup sets the cookie on its 303).
func (h *Harness) Signup(t *testing.T, name, email, password, accountType string) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	client := &http.Client{
		Timeout:       10 * time.Second,
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	form := url.Values{
		"name": {name}, "email": {email}, "password": {password}, "account_type": {accountType},
	}
	resp, err := client.Post(h.URLs.Gateway+"/v1/auth/signup", "application/x-www-form-urlencoded",
		strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("signup %s: %v", email, err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("signup %s: status %d (%s), want 303", email, resp.StatusCode, body)
	}
	return client
}

// apiJSON runs one JSON call on a session client and decodes the response.
// Fails the test on any status >= 400 so journey steps read linearly.
func (h *Harness) apiJSON(t *testing.T, client *http.Client, method, path, body string) map[string]any {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, h.URLs.Gateway+path, rdr)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		t.Fatalf("%s %s: status %d — %s", method, path, resp.StatusCode, raw)
	}
	out := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			// List endpoints return arrays; wrap them so callers can reach in.
			var arr []any
			if err2 := json.Unmarshal(raw, &arr); err2 != nil {
				t.Fatalf("%s %s: response not JSON: %v (%s)", method, path, err, raw)
			}
			out = map[string]any{"items": arr}
		}
	}
	return out
}

// BuildWorldViaAPI walks the full customer onboarding journey — the API
// twin of the SQL seed, usable as both a test fixture and a "seed via
// APIs" action:
//
//	publisher signup → create site → create placement
//	advertiser signup → create campaign (DSP attaches the account) → topup
//
// suffix keeps emails/names unique across runs against a shared stack.
// Callers that need deterministic isolation should h.Reset(t) first.
func (h *Harness) BuildWorldViaAPI(t *testing.T, suffix string) APIWorld {
	t.Helper()
	uniq := fmt.Sprintf("%s-%d", suffix, time.Now().UnixNano())
	w := APIWorld{}

	// Publisher side.
	w.Publisher = h.Signup(t, "API Pub "+suffix, "pub-"+uniq+"@api.test", "pw-e2e", "publisher")
	pub := h.apiJSON(t, w.Publisher, http.MethodPost, "/v1/api/publishers",
		fmt.Sprintf(`{"name":"API Site %s","domain":"%s.api.test"}`, suffix, uniq))
	w.PublisherID, _ = pub["id"].(string)
	if w.PublisherID == "" {
		t.Fatalf("publisher create returned no id: %v", pub)
	}
	pl := h.apiJSON(t, w.Publisher, http.MethodPost, "/v1/api/placements",
		fmt.Sprintf(`{"publisher_id":%q,"name":"API MPU %s","format":"display","width":300,"height":250,"floor_price":0.5}`, w.PublisherID, suffix))
	w.PlacementID, _ = pl["id"].(string)
	if w.PlacementID == "" {
		t.Fatalf("placement create returned no id: %v", pl)
	}

	// Advertiser side.
	w.Advertiser = h.Signup(t, "API Adv "+suffix, "adv-"+uniq+"@api.test", "pw-e2e", "advertiser")
	camp := h.apiJSON(t, w.Advertiser, http.MethodPost, "/v1/api/campaigns",
		fmt.Sprintf(`{"name":"API Campaign %s","base_bid":2.5,"daily_budget":100}`, suffix))
	w.CampaignID, _ = camp["id"].(string)
	if w.CampaignID == "" {
		t.Fatalf("campaign create returned no id: %v", camp)
	}
	h.apiJSON(t, w.Advertiser, http.MethodPost, "/v1/api/billing/topup",
		fmt.Sprintf(`{"amount":500,"idempotency_key":"apiworld-%s"}`, uniq))

	// Force the warm caches so the world is auction-ready immediately
	// instead of after the NATS invalidates land.
	h.RefreshAllCaches(t)
	return w
}
