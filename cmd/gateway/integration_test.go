package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
)

func cfgWithLive(kv map[string]string) *config.Config {
	c := config.Load()
	c.SetLiveBatch(kv)
	return c
}

// authedReq builds a GET carrying claims, since the handler 401s without them.
func authedReq() *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/api/integration/adstxt", nil)
	return r.WithContext(middleware.WithClaims(r.Context(), &auth.Claims{AccountID: "acct-1"}))
}

func TestIntegrationAdsTxt_Configured(t *testing.T) {
	cfg := cfgWithLive(map[string]string{
		"exchange.adstxt_seller_domain": "adtech.local",
		"exchange.adstxt_seller_id":     "adtech-exchange",
		"exchange.adstxt_enforcement":   "strict",
	})
	rr := httptest.NewRecorder()
	integrationAdsTxtHandler(cfg, quietLog()).ServeHTTP(rr, authedReq())
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var got adsTxtPolicyResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Configured {
		t.Error("expected configured=true")
	}
	if want := "adtech.local, adtech-exchange, DIRECT"; got.Line != want {
		t.Errorf("line = %q, want %q", got.Line, want)
	}
	if got.Enforcement != "strict" {
		t.Errorf("enforcement = %q, want strict", got.Enforcement)
	}
}

func TestIntegrationAdsTxt_Unconfigured(t *testing.T) {
	// No seller identity set → nothing to authorise; line must be empty.
	rr := httptest.NewRecorder()
	integrationAdsTxtHandler(cfgWithLive(nil), quietLog()).ServeHTTP(rr, authedReq())
	var got adsTxtPolicyResponse
	_ = json.NewDecoder(rr.Body).Decode(&got)
	if got.Configured || got.Line != "" {
		t.Errorf("expected unconfigured with empty line, got %+v", got)
	}
}

func TestIntegrationAdsTxt_RequiresAuth(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/api/integration/adstxt", nil) // no claims
	integrationAdsTxtHandler(cfgWithLive(nil), quietLog()).ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}
