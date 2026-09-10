package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/secrets"
)

type fakeKeyLookup struct {
	key string
	sec secrets.Secret
}

func (f fakeKeyLookup) LookupByValue(v string, _ time.Time) (secrets.Secret, bool) {
	if v == f.key {
		return f.sec, true
	}
	return secrets.Secret{}, false
}

func TestPartnerAuthGate(t *testing.T) {
	log := logger.New("test")
	cache := fakeKeyLookup{key: "sk_good", sec: secrets.Secret{AccountID: "acc-1", Purpose: secrets.PurposePartnerSandbox}}
	// A key that exists but is the wrong purpose must NOT authenticate.
	wrongPurpose := fakeKeyLookup{key: "sk_wrong", sec: secrets.Secret{AccountID: "acc-2", Purpose: secrets.PurposeAPIKey}}

	newReq := func(key string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/openrtb/auction", nil)
		if key != "" {
			r.Header.Set("X-API-Key", key)
		}
		return r
	}
	// next records whether it ran and what trusted account it saw.
	run := func(cache partnerKeyLookup, strict bool, key string) (ran bool, boundAcct string, status int) {
		next := func(w http.ResponseWriter, r *http.Request) {
			ran = true
			boundAcct = partnerAccountFromContext(r.Context())
			w.WriteHeader(http.StatusOK)
		}
		rec := httptest.NewRecorder()
		partnerAuthGate(next, cache, func() bool { return strict }, log)(rec, newReq(key))
		return ran, boundAcct, rec.Code
	}

	t.Run("valid key binds trusted account (strict)", func(t *testing.T) {
		ran, acct, code := run(cache, true, "sk_good")
		if !ran || acct != "acc-1" || code != http.StatusOK {
			t.Fatalf("ran=%v acct=%q code=%d, want true/acc-1/200", ran, acct, code)
		}
	})
	t.Run("missing key rejected in strict", func(t *testing.T) {
		ran, _, code := run(cache, true, "")
		if ran || code != http.StatusUnauthorized {
			t.Fatalf("ran=%v code=%d, want false/401", ran, code)
		}
	})
	t.Run("bad key rejected in strict", func(t *testing.T) {
		ran, _, code := run(cache, true, "sk_bad")
		if ran || code != http.StatusUnauthorized {
			t.Fatalf("ran=%v code=%d, want false/401", ran, code)
		}
	})
	t.Run("wrong-purpose key rejected in strict", func(t *testing.T) {
		ran, _, code := run(wrongPurpose, true, "sk_wrong")
		if ran || code != http.StatusUnauthorized {
			t.Fatalf("ran=%v code=%d, want false/401 (purpose must be partner_sandbox)", ran, code)
		}
	})
	t.Run("non-strict allows missing key (no binding)", func(t *testing.T) {
		ran, acct, code := run(cache, false, "")
		if !ran || acct != "" || code != http.StatusOK {
			t.Fatalf("ran=%v acct=%q code=%d, want true/empty/200", ran, acct, code)
		}
	})
	t.Run("non-strict still binds a valid key", func(t *testing.T) {
		ran, acct, _ := run(cache, false, "sk_good")
		if !ran || acct != "acc-1" {
			t.Fatalf("ran=%v acct=%q, want true/acc-1", ran, acct)
		}
	})
}
