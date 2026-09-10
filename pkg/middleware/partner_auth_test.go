package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/secrets"
)

type fakePartnerKeyLookup struct {
	key string
	sec secrets.Secret
}

func (f fakePartnerKeyLookup) LookupByValue(v string, _ time.Time) (secrets.Secret, bool) {
	if f.key != "" && v == f.key {
		return f.sec, true
	}
	return secrets.Secret{}, false
}

func TestPartnerInboundAuth(t *testing.T) {
	log := logger.New("test")
	good := fakePartnerKeyLookup{key: "sk_good", sec: secrets.Secret{AccountID: "acc-1", Purpose: secrets.PurposePartnerSandbox}}
	wrongPurpose := fakePartnerKeyLookup{key: "sk_wrong", sec: secrets.Secret{AccountID: "acc-2", Purpose: secrets.PurposeAPIKey}}

	// run drives the gate; returns whether next ran, the bound secret's account
	// (via the standard SecretFromContext), and the response status.
	run := func(cache APIKeyLookup, strict bool, key string) (ran bool, acct string, status int) {
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ran = true
			if s := SecretFromContext(r.Context()); s != nil {
				acct = s.AccountID
			}
			w.WriteHeader(http.StatusOK)
		})
		req := httptest.NewRequest(http.MethodPost, "/v1/openrtb/auction", nil)
		if key != "" {
			req.Header.Set("X-API-Key", key)
		}
		rec := httptest.NewRecorder()
		PartnerInboundAuth(cache, func() bool { return strict }, log)(next).ServeHTTP(rec, req)
		return ran, acct, rec.Code
	}

	cases := []struct {
		name       string
		cache      APIKeyLookup
		strict     bool
		key        string
		wantRan    bool
		wantAcct   string
		wantStatus int
	}{
		{"valid key binds secret (strict)", good, true, "sk_good", true, "acc-1", http.StatusOK},
		{"missing key rejected (strict)", good, true, "", false, "", http.StatusUnauthorized},
		{"bad key rejected (strict)", good, true, "sk_bad", false, "", http.StatusUnauthorized},
		{"wrong-purpose rejected (strict)", wrongPurpose, true, "sk_wrong", false, "", http.StatusUnauthorized},
		{"missing key allowed (warn)", good, false, "", true, "", http.StatusOK},
		{"invalid key allowed (warn)", good, false, "sk_bad", true, "", http.StatusOK},
		{"valid key still binds (warn)", good, false, "sk_good", true, "acc-1", http.StatusOK},
		{"nil cache: strict rejects", nil, true, "sk_good", false, "", http.StatusUnauthorized},
		{"nil cache: warn allows", nil, false, "sk_good", true, "", http.StatusOK},
		{"empty-account secret still authenticates", fakePartnerKeyLookup{key: "sk_e", sec: secrets.Secret{Purpose: secrets.PurposePartnerSandbox}}, true, "sk_e", true, "", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ran, acct, code := run(tc.cache, tc.strict, tc.key)
			if ran != tc.wantRan || acct != tc.wantAcct || code != tc.wantStatus {
				t.Errorf("ran=%v acct=%q code=%d; want ran=%v acct=%q code=%d", ran, acct, code, tc.wantRan, tc.wantAcct, tc.wantStatus)
			}
		})
	}
}
