package main

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adcert"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
)

func cfgWith(kv map[string]string) *config.Config {
	c := config.Load()
	c.SetLiveBatch(kv)
	return c
}

// keyServer stands in for the exchange's /v1/adcert/key endpoint.
func keyServer(t *testing.T, pubB64 string, status int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"alg": "Ed25519", "key": pubB64})
	}))
}

func TestAdCertKeyFetcher_FetchesAndVerifies(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	srv := keyServer(t, adcert.EncodeKey(pub), http.StatusOK)
	defer srv.Close()

	f := newAdCertKeyFetcher(srv.URL, time.Minute, quietMgmtLog())
	if f.Current() != nil {
		t.Fatal("expected no key before fetch")
	}
	f.fetch() // direct, deterministic (no goroutine/ticker)

	got := f.Current()
	if got == nil {
		t.Fatal("expected a fetched key")
	}
	// The fetched key must verify a signature made with the matching private key.
	req := &openrtb.BidRequest{ID: "trace-1", Imp: []openrtb.Imp{{TagID: "pl-1"}}}
	if !adcert.VerifyAny(got, req, adcert.Sign(priv, req)) {
		t.Error("fetched keyset failed to verify a genuine signature")
	}
}

func TestAdCertKeyFetcher_KeepsLastKeyOnFailure(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	okSrv := keyServer(t, adcert.EncodeKey(pub), http.StatusOK)
	f := newAdCertKeyFetcher(okSrv.URL, time.Minute, quietMgmtLog())
	f.fetch()
	okSrv.Close() // now the endpoint is unreachable

	f.fetch() // should log + keep the last good key, not clear it
	if f.Current() == nil {
		t.Error("fetcher dropped the last-good key on a failed refresh")
	}
}

func TestAdCertKeySource_StaticFallback(t *testing.T) {
	// With no key_url and a static verify key, the source returns that key.
	pub, _, _ := ed25519.GenerateKey(nil)
	cfg := cfgWith(map[string]string{"dsp.adcert_verify_key": adcert.EncodeKey(pub)})
	keyFn := adCertKeySource(cfg, quietMgmtLog(), func(string, func()) {})
	if got := keyFn(); len(got) == 0 {
		t.Fatal("expected the static key")
	}
}
