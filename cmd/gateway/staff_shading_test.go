package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

func TestStaffShadingHandler(t *testing.T) {
	staff := &auth.Claims{UserID: "u1", AccountType: auth.AccountStaff, Permissions: []string{"support:read"}}
	adv := &auth.Claims{UserID: "u2", AccountID: "adv-1", AccountType: auth.AccountAdvertiser, Permissions: []string{"campaigns:read"}}

	// Fake DSP serving the internal shading endpoint with the real response
	// shape (map[placement_id]bidshading.PlacementStats, Go field names).
	dsp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != routes.DSPShading {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"pl-1":{"TotalBids":10,"Wins":4,"Losses":6,"WinRate":0.4,"AvgClearing":2.35,"BelowFloor":1,"Outbid":5}}`))
	}))
	defer dsp.Close()

	// Staff GET → 200 with the DSP body passed through verbatim.
	rec := httptest.NewRecorder()
	staffShadingHandler(dsp.URL, quietLog())(rec, withClaims(httptest.NewRequest(http.MethodGet, "/v1/api/staff/shading", nil), staff))
	if rec.Code != http.StatusOK {
		t.Fatalf("staff GET code=%d body=%s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{`"pl-1"`, `"TotalBids":10`, `"WinRate":0.4`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("shading body missing %s: %s", want, rec.Body.String())
		}
	}

	// Advertiser → 403 (staff-only surface).
	rec = httptest.NewRecorder()
	staffShadingHandler(dsp.URL, quietLog())(rec, withClaims(httptest.NewRequest(http.MethodGet, "/v1/api/staff/shading", nil), adv))
	if rec.Code != http.StatusForbidden {
		t.Errorf("advertiser GET code=%d, want 403", rec.Code)
	}

	// POST → 405 (read-only; shading is model-driven, no knobs).
	rec = httptest.NewRecorder()
	staffShadingHandler(dsp.URL, quietLog())(rec, withClaims(httptest.NewRequest(http.MethodPost, "/v1/api/staff/shading", nil), staff))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST code=%d, want 405", rec.Code)
	}

	// DSP unreachable → 502.
	dead := httptest.NewServer(nil)
	dead.Close()
	rec = httptest.NewRecorder()
	staffShadingHandler(dead.URL, quietLog())(rec, withClaims(httptest.NewRequest(http.MethodGet, "/v1/api/staff/shading", nil), staff))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("dead-DSP GET code=%d, want 502", rec.Code)
	}
}
