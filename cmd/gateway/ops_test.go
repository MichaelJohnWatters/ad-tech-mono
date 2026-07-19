package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/audit"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/kubeops"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
)

// newOpsTestAPI wires an opsAPI at a fake k8s API server (real kubeops client,
// httptest transport — no k8s mock) and captures audit entries.
func newOpsTestAPI(t *testing.T, apiMux *http.ServeMux) (*opsAPI, *[]audit.Entry) {
	t.Helper()
	srv := httptest.NewServer(apiMux)
	t.Cleanup(srv.Close)
	kube := kubeops.NewWithClient(srv.URL, "adtech", "tok", srv.Client())
	var entries []audit.Entry
	auditFn := func(_ context.Context, e audit.Entry) { entries = append(entries, e) }
	return newOpsAPI(kube, nil, "", auditFn, quietLog()), &entries
}

func opsClaims(perms ...string) *auth.Claims {
	return &auth.Claims{UserID: "user-ops", AccountType: auth.AccountStaff, Role: auth.RoleDevOps, Permissions: perms}
}

func TestOpsPodsHandler(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/namespaces/adtech/pods", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"gateway-1","labels":{"app":"gateway"}},
			"spec":{"containers":[{"image":"img"}]},
			"status":{"phase":"Running","containerStatuses":[{"ready":true,"restartCount":0}]}}]}`))
	})
	ops, _ := newOpsTestAPI(t, mux)

	rec := httptest.NewRecorder()
	ops.podsHandler(rec, withClaims(httptest.NewRequest(http.MethodGet, "/v1/api/ops/pods", nil), opsClaims("ops:read")))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "gateway-1") {
		t.Fatalf("pods code=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"namespace":"adtech"`) {
		t.Errorf("namespace missing: %s", rec.Body.String())
	}
}

// A nil kube client (gateway off-cluster) degrades to 503 on every k8s-backed
// endpoint rather than panicking.
func TestOpsHandlersOffCluster503(t *testing.T) {
	ops := newOpsAPI(nil, nil, "", func(context.Context, audit.Entry) {}, quietLog())
	claims := opsClaims("ops:read", "ops:deploy")
	for name, tc := range map[string]struct {
		method, path, body string
		h                  http.HandlerFunc
	}{
		"pods":     {http.MethodGet, "/v1/api/ops/pods", "", ops.podsHandler},
		"cronjobs": {http.MethodGet, "/v1/api/ops/cronjobs", "", ops.cronJobsHandler},
		"jobs":     {http.MethodGet, "/v1/api/ops/jobs", "", ops.jobsHandler},
		"pvcs":     {http.MethodGet, "/v1/api/ops/pvcs", "", ops.pvcsHandler},
		"logs":     {http.MethodGet, "/v1/api/ops/logs?pod=x", "", ops.logsHandler},
		"restart":  {http.MethodPost, "/v1/api/ops/restart", `{"deployment":"gateway"}`, ops.restartHandler},
		"trigger":  {http.MethodPost, "/v1/api/ops/cronjobs/trigger", `{"name":"adstxt"}`, ops.cronJobTriggerHandler},
	} {
		rec := httptest.NewRecorder()
		tc.h(rec, withClaims(httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)), claims))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s off-cluster: code=%d, want 503", name, rec.Code)
		}
	}
}

func TestOpsReadyzGridHandler(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer up.Close()
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "redis unreachable", http.StatusServiceUnavailable)
	}))
	defer down.Close()

	ops := newOpsAPI(nil, []opsTarget{{Name: "up-svc", URL: up.URL}, {Name: "down-svc", URL: down.URL}},
		"", func(context.Context, audit.Entry) {}, quietLog())
	rec := httptest.NewRecorder()
	ops.readyzGridHandler(rec, withClaims(httptest.NewRequest(http.MethodGet, "/v1/api/ops/readyz-grid", nil), opsClaims("ops:read")))
	if rec.Code != http.StatusOK {
		t.Fatalf("readyz-grid code=%d", rec.Code)
	}
	var out struct {
		Services []readyzView `json:"services"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Services) != 2 {
		t.Fatalf("got %d services", len(out.Services))
	}
	if !out.Services[0].OK || out.Services[0].Status != 200 {
		t.Errorf("up-svc: %+v", out.Services[0])
	}
	if out.Services[1].OK || out.Services[1].Status != 503 || !strings.Contains(out.Services[1].Body, "redis unreachable") {
		t.Errorf("down-svc: %+v", out.Services[1])
	}
}

func TestOpsNATSHandler(t *testing.T) {
	monitor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/jsz" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"streams":1,"consumers":2,"messages":100,"bytes":2048,
		  "account_details":[{"stream_detail":[
		    {"name":"ADTECH","state":{"messages":100,"bytes":2048,"consumer_count":2},
		     "consumer_detail":[
		       {"name":"reporting-impression","num_pending":0,"num_ack_pending":0},
		       {"name":"billing-win","num_pending":42,"num_ack_pending":3,"num_redelivered":1}]}]}]}`))
	}))
	defer monitor.Close()

	ops := newOpsAPI(nil, nil, monitor.URL, func(context.Context, audit.Entry) {}, quietLog())
	rec := httptest.NewRecorder()
	ops.natsHandler(rec, withClaims(httptest.NewRequest(http.MethodGet, "/v1/api/ops/nats", nil), opsClaims("ops:read")))
	if rec.Code != http.StatusOK {
		t.Fatalf("nats code=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"ADTECH"`) || !strings.Contains(body, "billing-win") {
		t.Errorf("nats body missing stream/lagging consumer: %s", body)
	}
	if strings.Contains(body, "reporting-impression") {
		t.Errorf("caught-up consumer should not be in lagging list: %s", body)
	}

	// Monitor down → 503.
	opsDown := newOpsAPI(nil, nil, "http://127.0.0.1:1", func(context.Context, audit.Entry) {}, quietLog())
	rec = httptest.NewRecorder()
	opsDown.natsHandler(rec, withClaims(httptest.NewRequest(http.MethodGet, "/v1/api/ops/nats", nil), opsClaims("ops:read")))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("nats monitor down: code=%d, want 503", rec.Code)
	}
}

func TestOpsLogsHandler(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/namespaces/adtech/pods/gateway-1/log", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("tailLines") != "200" { // default tail
			t.Errorf("tailLines = %q, want default 200", r.URL.Query().Get("tailLines"))
		}
		_, _ = w.Write([]byte("log line\n"))
	})
	ops, _ := newOpsTestAPI(t, mux)

	rec := httptest.NewRecorder()
	ops.logsHandler(rec, withClaims(httptest.NewRequest(http.MethodGet, "/v1/api/ops/logs?pod=gateway-1", nil), opsClaims("ops:read")))
	if rec.Code != http.StatusOK || rec.Body.String() != "log line\n" {
		t.Fatalf("logs code=%d body=%q", rec.Code, rec.Body.String())
	}

	// Bad pod names / tails are rejected before touching the API.
	for _, target := range []string{
		"/v1/api/ops/logs",                         // missing pod
		"/v1/api/ops/logs?pod=../secrets",          // path traversal shape
		"/v1/api/ops/logs?pod=gateway-1&tail=0",    // tail below range
		"/v1/api/ops/logs?pod=gateway-1&tail=9999", // tail above range
	} {
		rec := httptest.NewRecorder()
		ops.logsHandler(rec, withClaims(httptest.NewRequest(http.MethodGet, target, nil), opsClaims("ops:read")))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: code=%d, want 400", target, rec.Code)
		}
	}
}

func TestOpsRestartHandlerAudits(t *testing.T) {
	patched := false
	mux := http.NewServeMux()
	mux.HandleFunc("/apis/apps/v1/namespaces/adtech/deployments/reporting", func(w http.ResponseWriter, r *http.Request) {
		patched = r.Method == http.MethodPatch
		_, _ = w.Write([]byte(`{}`))
	})
	ops, entries := newOpsTestAPI(t, mux)

	rec := httptest.NewRecorder()
	ops.restartHandler(rec, withClaims(httptest.NewRequest(http.MethodPost, "/v1/api/ops/restart",
		strings.NewReader(`{"deployment":"reporting"}`)), opsClaims("ops:deploy")))
	if rec.Code != http.StatusOK || !patched {
		t.Fatalf("restart code=%d patched=%v body=%s", rec.Code, patched, rec.Body.String())
	}
	if len(*entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(*entries))
	}
	e := (*entries)[0]
	if e.Action != "ops:restart_deployment" || e.ResourceType != "deployment" || e.ResourceID != "reporting" || e.ActorID != "user-ops" {
		t.Errorf("audit entry wrong: %+v", e)
	}

	// Invalid deployment name → 400, no audit.
	rec = httptest.NewRecorder()
	ops.restartHandler(rec, withClaims(httptest.NewRequest(http.MethodPost, "/v1/api/ops/restart",
		strings.NewReader(`{"deployment":"Bad_Name!"}`)), opsClaims("ops:deploy")))
	if rec.Code != http.StatusBadRequest || len(*entries) != 1 {
		t.Errorf("bad name: code=%d entries=%d", rec.Code, len(*entries))
	}
}

func TestOpsCronJobTriggerHandlerAudits(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/apis/batch/v1/namespaces/adtech/cronjobs/batch-conductor", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"spec":{"jobTemplate":{"spec":{"template":{"spec":{"containers":[{"name":"c","image":"i"}]}}}}}}`))
	})
	mux.HandleFunc("/apis/batch/v1/namespaces/adtech/jobs", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	})
	ops, entries := newOpsTestAPI(t, mux)

	rec := httptest.NewRecorder()
	ops.cronJobTriggerHandler(rec, withClaims(httptest.NewRequest(http.MethodPost, "/v1/api/ops/cronjobs/trigger",
		strings.NewReader(`{"name":"batch-conductor"}`)), opsClaims("ops:deploy")))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "batch-conductor-manual-") {
		t.Fatalf("trigger code=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(*entries) != 1 || (*entries)[0].Action != "ops:trigger_cronjob" || (*entries)[0].ResourceID != "batch-conductor" {
		t.Errorf("audit entries = %+v", *entries)
	}
}

// The wiring contract: reads sit behind ops:read, mutations behind ops:deploy
// — a devops session passes, a support-only session 403s on both.
func TestOpsPermissionGating(t *testing.T) {
	ops := newOpsAPI(nil, nil, "", func(context.Context, audit.Entry) {}, quietLog())
	support := &auth.Claims{UserID: "u", Permissions: []string{"support:read"}}

	read := middleware.RequirePermission("ops:read")(http.HandlerFunc(ops.podsHandler))
	rec := httptest.NewRecorder()
	read.ServeHTTP(rec, withClaims(httptest.NewRequest(http.MethodGet, "/v1/api/ops/pods", nil), support))
	if rec.Code != http.StatusForbidden {
		t.Errorf("support-only GET: code=%d, want 403", rec.Code)
	}

	deploy := middleware.RequirePermission("ops:deploy")(http.HandlerFunc(ops.restartHandler))
	rec = httptest.NewRecorder()
	deploy.ServeHTTP(rec, withClaims(httptest.NewRequest(http.MethodPost, "/v1/api/ops/restart",
		strings.NewReader(`{"deployment":"x"}`)), opsClaims("ops:read")))
	if rec.Code != http.StatusForbidden {
		t.Errorf("read-only POST: code=%d, want 403", rec.Code)
	}

	// staff:devops role carries both.
	devopsPerms := auth.RolePermissions(auth.AccountStaff, auth.RoleDevOps)
	c := &auth.Claims{UserID: "u", Permissions: devopsPerms}
	if !auth.HasPermission(c, "ops:read") || !auth.HasPermission(c, "ops:deploy") {
		t.Errorf("staff:devops perms missing ops grants: %v", devopsPerms)
	}
}

func TestNATSMonitorURLFrom(t *testing.T) {
	for in, want := range map[string]string{
		"nats://nats:4222":      "http://nats:8222",
		"nats://localhost:4222": "http://localhost:8222",
		"":                      "",
	} {
		if got := natsMonitorURLFrom(in); got != want {
			t.Errorf("natsMonitorURLFrom(%q) = %q, want %q", in, got, want)
		}
	}
}
