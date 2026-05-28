package health_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/health"
)

func TestLivenessHandler(t *testing.T) {
	h := health.New()
	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()

	h.LivenessHandler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("liveness status = %d, want 200", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["status"] != "alive" {
		t.Errorf("status = %q, want alive", resp["status"])
	}
}

func TestReadinessHandler_AllHealthy(t *testing.T) {
	h := health.New()
	h.AddReadinessCheck("postgres", func(ctx context.Context) error { return nil })
	h.AddReadinessCheck("redis", func(ctx context.Context) error { return nil })

	req := httptest.NewRequest("GET", "/readyz", nil)
	w := httptest.NewRecorder()

	h.ReadinessHandler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("readiness status = %d, want 200", w.Code)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["status"] != "ready" {
		t.Errorf("status = %v, want ready", resp["status"])
	}
}

func TestReadinessHandler_OneUnhealthy(t *testing.T) {
	h := health.New()
	h.AddReadinessCheck("postgres", func(ctx context.Context) error { return nil })
	h.AddReadinessCheck("redis", func(ctx context.Context) error {
		return errors.New("connection refused")
	})

	req := httptest.NewRequest("GET", "/readyz", nil)
	w := httptest.NewRecorder()

	h.ReadinessHandler().ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("readiness status = %d, want 503", w.Code)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["status"] != "not_ready" {
		t.Errorf("status = %v, want not_ready", resp["status"])
	}

	checks := resp["checks"].(map[string]any)
	if checks["postgres"] != "ok" {
		t.Errorf("postgres check = %v, want ok", checks["postgres"])
	}
	if checks["redis"] != "connection refused" {
		t.Errorf("redis check = %v, want 'connection refused'", checks["redis"])
	}
}

func TestReadinessHandler_NoChecks(t *testing.T) {
	h := health.New()

	req := httptest.NewRequest("GET", "/readyz", nil)
	w := httptest.NewRecorder()

	h.ReadinessHandler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("readiness with no checks should be 200, got %d", w.Code)
	}
}
