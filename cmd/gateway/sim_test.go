package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/simulator/request"
)

// TestSimRealismEndpoint asserts the /v1/sim/realism endpoint emits exactly the
// consent/identity encoding from pkg/simulator/request — proving the web UI and
// the CLI share one source of truth for the fiddly TCF/GPP/UID2 encoding.
func TestSimRealismEndpoint(t *testing.T) {
	cases := []struct {
		consent  string
		identity string
		wantKeys []string
	}{
		{"gdpr_consented", "uid2", []string{"gdpr", "consent", "uid2"}},
		{"gdpr_no_consent", "anonymous", []string{"gdpr"}},
		{"ccpa_opt_out", "publisher_id", []string{"us_privacy", "user_id"}},
		{"gpc", "hashed_email", []string{"gpc", "hashed_email"}},
		{"coppa", "anonymous", []string{"coppa"}},
		{"gpp_opt_out", "uid2", []string{"gpp", "gpp_sid", "uid2"}},
		{"us_clear", "uid2", []string{"us_privacy", "uid2"}},
	}
	for _, c := range cases {
		t.Run(c.consent+"/"+c.identity, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/v1/sim/realism?consent="+c.consent+"&identity="+c.identity, nil)
			simRealismHandler(rec, req)

			if rec.Code != 200 {
				t.Fatalf("status = %d", rec.Code)
			}
			var out struct {
				Query string `json:"query"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("decode: %v", err)
			}
			got, err := url.ParseQuery(out.Query)
			if err != nil {
				t.Fatalf("returned query is not parseable: %v", err)
			}
			for _, k := range c.wantKeys {
				if got.Get(k) == "" {
					t.Errorf("regime %s / identity %s: missing param %q in %q", c.consent, c.identity, k, out.Query)
				}
			}

			// The endpoint output must equal the Go source exactly (single source
			// of truth: the same seed → the same encoding).
			seed := c.consent + "|" + c.identity
			want := request.RealismParams(request.Regime(c.consent), request.Identity(c.identity), seed).Encode()
			if out.Query != want {
				t.Errorf("endpoint diverged from pkg/simulator/request:\n got=%s\nwant=%s", out.Query, want)
			}
		})
	}
}

// TestSimPersonasEndpoint asserts the personas endpoint returns the registry.
func TestSimPersonasEndpoint(t *testing.T) {
	rec := httptest.NewRecorder()
	simPersonasHandler(rec, httptest.NewRequest(http.MethodGet, "/v1/sim/personas", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "us-personalised-mobile") {
		t.Errorf("personas response missing a known persona: %s", rec.Body.String())
	}
}
