package middleware

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/secrets"
)

// stubLookup satisfies APIKeyLookup with a static map. Lets us drive
// AuthAPIKey without spinning up a real warm cache.
type stubLookup map[string]secrets.Secret

func (s stubLookup) LookupByValue(v string, now time.Time) (secrets.Secret, bool) {
	sec, ok := s[v]
	if !ok || !sec.IsAcceptable(now) {
		return secrets.Secret{}, false
	}
	return sec, true
}

func silentLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestAuthAPIKey(t *testing.T) {
	expired := time.Now().Add(-1 * time.Hour)
	future := time.Now().Add(1 * time.Hour)

	store := stubLookup{
		"valid-active":     {Name: "ops-key-1", Value: "valid-active", Purpose: secrets.PurposeAPIKey, Status: secrets.StatusActive},
		"valid-rotating":   {Name: "ops-key-2", Value: "valid-rotating", Purpose: secrets.PurposeAPIKey, Status: secrets.StatusRotating},
		"revoked-key":      {Name: "ops-key-3", Value: "revoked-key", Purpose: secrets.PurposeAPIKey, Status: secrets.StatusRevoked},
		"expired-key":      {Name: "ops-key-4", Value: "expired-key", Purpose: secrets.PurposeAPIKey, Status: secrets.StatusActive, ExpiresAt: &expired},
		"future-expiry":    {Name: "ops-key-5", Value: "future-expiry", Purpose: secrets.PurposeAPIKey, Status: secrets.StatusActive, ExpiresAt: &future},
		"jwt-signing-coll": {Name: "jwt-1", Value: "jwt-signing-coll", Purpose: secrets.PurposeJWTSigning, Status: secrets.StatusActive},
	}

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Echo the matched secret's name so the test can assert context wiring.
		s := SecretFromContext(r.Context())
		if s != nil {
			w.Header().Set("X-Auth-Name", s.Name)
		}
		w.WriteHeader(http.StatusOK)
	})
	h := AuthAPIKey(store, silentLog())(next)

	cases := []struct {
		name       string
		key        string
		wantStatus int
		wantName   string
	}{
		{"no header", "", http.StatusUnauthorized, ""},
		{"unknown key", "totally-unknown", http.StatusUnauthorized, ""},
		{"active key passes", "valid-active", http.StatusOK, "ops-key-1"},
		{"rotating key still accepted (grace window)", "valid-rotating", http.StatusOK, "ops-key-2"},
		{"revoked key rejected", "revoked-key", http.StatusUnauthorized, ""},
		{"expired key rejected", "expired-key", http.StatusUnauthorized, ""},
		{"future-expiry key passes", "future-expiry", http.StatusOK, "ops-key-5"},
		{"jwt-signing collision rejected (purpose mismatch)", "jwt-signing-coll", http.StatusUnauthorized, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/test", nil)
			if tc.key != "" {
				req.Header.Set("X-API-Key", tc.key)
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != tc.wantStatus {
				t.Errorf("status: got %d, want %d", rr.Code, tc.wantStatus)
			}
			if got := rr.Header().Get("X-Auth-Name"); got != tc.wantName {
				t.Errorf("X-Auth-Name: got %q, want %q", got, tc.wantName)
			}
		})
	}
}

// TestAuthAPIKey_QueryParamFallback exercises the browser-navigation
// fallback added so dev quick-links in the pub sim work without a
// header injector. Header is preferred; ?api_key= only fires when the
// header is absent and only on GETs.
func TestAuthAPIKey_QueryParamFallback(t *testing.T) {
	store := stubLookup{
		"valid-active": {Name: "ops-key-1", Value: "valid-active", Purpose: secrets.PurposeAPIKey, Status: secrets.StatusActive},
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := AuthAPIKey(store, silentLog())(next)

	t.Run("GET with ?api_key= passes", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/test?api_key=valid-active", nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("status: got %d, want 200", rr.Code)
		}
	})

	t.Run("POST with ?api_key= rejected (query-param fallback is GET-only)", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/test?api_key=valid-active", nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("status: got %d, want 401 (query fallback should not fire on POST)", rr.Code)
		}
	})

	t.Run("header takes precedence over query param", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/test?api_key=valid-active", nil)
		req.Header.Set("X-API-Key", "totally-unknown")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("status: got %d, want 401 (bad header must win even if query is valid)", rr.Code)
		}
	})
}

func TestTruncatedKey(t *testing.T) {
	if got := truncatedKey("abcd"); got != "****" {
		t.Errorf("short key masking: got %q, want ****", got)
	}
	if got := truncatedKey("abcdefghij"); got != "abcdefgh…" {
		t.Errorf("long key truncation: got %q, want abcdefgh…", got)
	}
}
