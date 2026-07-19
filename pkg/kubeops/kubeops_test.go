package kubeops

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeAPI is a minimal API-server stand-in: routes the paths the client hits,
// records the last request, and lets tests inject bodies/statuses.
type fakeAPI struct {
	t          *testing.T
	mux        *http.ServeMux
	lastMethod string
	lastPath   string
	lastCT     string
	lastBody   []byte
}

func newFakeAPI(t *testing.T) (*fakeAPI, *Client) {
	f := &fakeAPI{t: t, mux: http.NewServeMux()}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("missing bearer token, got %q", got)
		}
		f.lastMethod, f.lastPath, f.lastCT = r.Method, r.URL.RequestURI(), r.Header.Get("Content-Type")
		f.lastBody, _ = io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(f.lastBody))
		f.mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	c := NewWithClient(srv.URL, "adtech", "test-token", srv.Client())
	c.now = func() time.Time { return time.Unix(1700000000, 0).UTC() }
	return f, c
}

func (f *fakeAPI) respond(pattern, body string) {
	f.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
}

func TestListPods(t *testing.T) {
	f, c := newFakeAPI(t)
	f.respond("/api/v1/namespaces/adtech/pods", `{"items":[
	  {"metadata":{"name":"gateway-abc","labels":{"app":"gateway"}},
	   "spec":{"containers":[{"image":"adtech-gateway:dev"}]},
	   "status":{"phase":"Running","startTime":"2026-07-01T00:00:00Z",
	     "containerStatuses":[{"ready":true,"restartCount":2}]}},
	  {"metadata":{"name":"dsp-xyz","labels":{"app":"dsp"}},
	   "spec":{"containers":[{"image":"adtech-dsp:dev"}]},
	   "status":{"phase":"Pending","containerStatuses":[{"ready":false,"restartCount":0}]}}]}`)

	pods, err := c.ListPods(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(pods) != 2 {
		t.Fatalf("got %d pods", len(pods))
	}
	gw := pods[0]
	if gw.Name != "gateway-abc" || gw.App != "gateway" || !gw.Ready || gw.RestartCount != 2 ||
		gw.Phase != "Running" || gw.Image != "adtech-gateway:dev" || gw.StartTime == nil {
		t.Errorf("gateway pod parsed wrong: %+v", gw)
	}
	if pods[1].Ready {
		t.Error("dsp pod should not be ready")
	}
}

func TestRestartDeployment(t *testing.T) {
	f, c := newFakeAPI(t)
	f.respond("/apis/apps/v1/namespaces/adtech/deployments/gateway", `{}`)

	if err := c.RestartDeployment(context.Background(), "gateway"); err != nil {
		t.Fatal(err)
	}
	if f.lastMethod != http.MethodPatch {
		t.Errorf("method = %s", f.lastMethod)
	}
	if f.lastCT != "application/strategic-merge-patch+json" {
		t.Errorf("content-type = %s", f.lastCT)
	}
	var patch struct {
		Spec struct {
			Template struct {
				Metadata struct {
					Annotations map[string]string `json:"annotations"`
				} `json:"metadata"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(f.lastBody, &patch); err != nil {
		t.Fatalf("patch body: %v", err)
	}
	at := patch.Spec.Template.Metadata.Annotations["kubectl.kubernetes.io/restartedAt"]
	if _, err := time.Parse(time.RFC3339, at); err != nil {
		t.Errorf("restartedAt %q not RFC3339: %v", at, err)
	}
}

func TestPodLogs(t *testing.T) {
	f, c := newFakeAPI(t)
	f.mux.HandleFunc("/api/v1/namespaces/adtech/pods/gateway-abc/log", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("tailLines") != "50" || r.URL.Query().Get("container") != "gw" {
			t.Errorf("log query = %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte("line1\nline2\n"))
	})

	logs, err := c.PodLogs(context.Background(), "gateway-abc", "gw", 50)
	if err != nil {
		t.Fatal(err)
	}
	if logs != "line1\nline2\n" {
		t.Errorf("logs = %q", logs)
	}
}

func TestListCronJobs(t *testing.T) {
	f, c := newFakeAPI(t)
	f.respond("/apis/batch/v1/namespaces/adtech/cronjobs", `{"items":[
	  {"metadata":{"name":"batch-conductor"},
	   "spec":{"schedule":"10 * * * *","suspend":false},
	   "status":{"lastScheduleTime":"2026-07-17T10:10:00Z"}},
	  {"metadata":{"name":"adstxt"},"spec":{"schedule":"0 3 * * *","suspend":true},"status":{}}]}`)

	cjs, err := c.ListCronJobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cjs) != 2 {
		t.Fatalf("got %d cronjobs", len(cjs))
	}
	if cjs[0].Name != "batch-conductor" || cjs[0].Schedule != "10 * * * *" || cjs[0].Suspend || cjs[0].LastScheduleTime == nil {
		t.Errorf("cronjob parsed wrong: %+v", cjs[0])
	}
	if !cjs[1].Suspend || cjs[1].LastScheduleTime != nil {
		t.Errorf("suspended cronjob parsed wrong: %+v", cjs[1])
	}
}

func TestTriggerCronJob(t *testing.T) {
	f, c := newFakeAPI(t)
	f.respond("/apis/batch/v1/namespaces/adtech/cronjobs/batch-conductor", `{
	  "spec":{"jobTemplate":{"spec":{"backoffLimit":1,"template":{"spec":{"containers":[{"name":"batch-conductor","image":"adtech-batch-conductor"}]}}}}}}`)
	var created []byte
	f.mux.HandleFunc("/apis/batch/v1/namespaces/adtech/jobs", func(w http.ResponseWriter, r *http.Request) {
		created, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	})

	name, err := c.TriggerCronJob(context.Background(), "batch-conductor")
	if err != nil {
		t.Fatal(err)
	}
	if name != "batch-conductor-manual-1700000000" {
		t.Errorf("job name = %s", name)
	}
	var job struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Name   string            `json:"name"`
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
		Spec struct {
			BackoffLimit int `json:"backoffLimit"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(created, &job); err != nil {
		t.Fatalf("created job body: %v", err)
	}
	if job.Kind != "Job" || job.Metadata.Name != name || job.Metadata.Labels[ManualTriggerLabel] != "true" {
		t.Errorf("job metadata wrong: %+v", job)
	}
	if job.Spec.BackoffLimit != 1 {
		t.Error("jobTemplate spec not carried through")
	}
}

func TestListJobs(t *testing.T) {
	f, c := newFakeAPI(t)
	f.mux.HandleFunc("/apis/batch/v1/namespaces/adtech/jobs", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("labelSelector") != ManualTriggerLabel {
			t.Errorf("labelSelector = %q", r.URL.Query().Get("labelSelector"))
		}
		_, _ = w.Write([]byte(`{"items":[
		  {"metadata":{"name":"batch-conductor-manual-1"},
		   "status":{"succeeded":1,"startTime":"2026-07-17T11:00:00Z"}},
		  {"metadata":{"name":"seed-x"},"status":{"failed":2,"active":1}}]}`))
	})

	jobs, err := c.ListJobs(context.Background(), ManualTriggerLabel)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 {
		t.Fatalf("got %d jobs", len(jobs))
	}
	if jobs[0].Succeeded != 1 || jobs[0].StartTime == nil {
		t.Errorf("job parsed wrong: %+v", jobs[0])
	}
	if jobs[1].Failed != 2 || jobs[1].Active != 1 {
		t.Errorf("job parsed wrong: %+v", jobs[1])
	}
}

func TestListPVCs(t *testing.T) {
	f, c := newFakeAPI(t)
	f.respond("/api/v1/namespaces/adtech/persistentvolumeclaims", `{"items":[
	  {"metadata":{"name":"postgres-data"},
	   "spec":{"resources":{"requests":{"storage":"10Gi"}}},
	   "status":{"phase":"Bound"}}]}`)

	pvcs, err := c.ListPVCs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(pvcs) != 1 || pvcs[0].Name != "postgres-data" || pvcs[0].Phase != "Bound" || pvcs[0].Requested != "10Gi" {
		t.Errorf("pvcs = %+v", pvcs)
	}
}

// Non-200 statuses become errors carrying the server message, for every helper.
func TestNon200sAreErrors(t *testing.T) {
	_, c := newFakeAPI(t) // no routes registered → mux 404s everything

	ctx := context.Background()
	checks := []struct {
		name string
		call func() error
	}{
		{"ListPods", func() error { _, err := c.ListPods(ctx); return err }},
		{"RestartDeployment", func() error { return c.RestartDeployment(ctx, "gateway") }},
		{"PodLogs", func() error { _, err := c.PodLogs(ctx, "p", "", 10); return err }},
		{"ListCronJobs", func() error { _, err := c.ListCronJobs(ctx); return err }},
		{"TriggerCronJob", func() error { _, err := c.TriggerCronJob(ctx, "x"); return err }},
		{"ListJobs", func() error { _, err := c.ListJobs(ctx, ""); return err }},
		{"ListPVCs", func() error { _, err := c.ListPVCs(ctx); return err }},
	}
	for _, tc := range checks {
		err := tc.call()
		if err == nil {
			t.Errorf("%s: expected error on 404", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), "status 404") {
			t.Errorf("%s: error should carry the status, got %v", tc.name, err)
		}
	}
}

func TestNewOffClusterErrors(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	if c, err := New(); err == nil || c != nil {
		t.Errorf("New off-cluster: client=%v err=%v, want (nil, err)", c, err)
	}
}
