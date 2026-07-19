// Package kubeops is a minimal in-cluster Kubernetes REST client for the
// staff ops console. Hand-rolled HTTP against the API server (same thin-client
// approach as the rest of the repo) — client-go would drag in a dependency
// tree for the handful of list/patch/create calls the console needs.
//
// New() reads the standard in-cluster service-account material
// (/var/run/secrets/kubernetes.io/serviceaccount/*) and the
// KUBERNETES_SERVICE_HOST/PORT env vars; off-cluster it returns an error and
// callers degrade to 503.
package kubeops

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"
)

const serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// ManualTriggerLabel marks Jobs created by TriggerCronJob so the console can
// list "runs I kicked off" separately from scheduled ones.
const ManualTriggerLabel = "adtech.dev/manual-trigger"

// Client talks to the Kubernetes API server for a single namespace.
type Client struct {
	baseURL   string
	namespace string
	token     string
	http      *http.Client
	now       func() time.Time
}

// New builds an in-cluster client from the pod's service-account mount.
// Returns an error when not running in a cluster (missing env or files).
func New() (*Client, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, fmt.Errorf("kubeops: not in cluster (KUBERNETES_SERVICE_HOST/PORT unset)")
	}
	token, err := os.ReadFile(serviceAccountDir + "/token")
	if err != nil {
		return nil, fmt.Errorf("kubeops: read serviceaccount token: %w", err)
	}
	namespace, err := os.ReadFile(serviceAccountDir + "/namespace")
	if err != nil {
		return nil, fmt.Errorf("kubeops: read serviceaccount namespace: %w", err)
	}
	caCert, err := os.ReadFile(serviceAccountDir + "/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("kubeops: read serviceaccount ca.crt: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("kubeops: ca.crt contains no certificates")
	}
	hc := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool},
		},
	}
	return NewWithClient("https://"+host+":"+port, string(bytes.TrimSpace(namespace)), string(bytes.TrimSpace(token)), hc), nil
}

// NewWithClient builds a client against an explicit API server — the test
// seam (httptest server) and any future kubeconfig-style use.
func NewWithClient(baseURL, namespace, token string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{baseURL: baseURL, namespace: namespace, token: token, http: hc, now: time.Now}
}

// Namespace returns the namespace the client operates in.
func (c *Client) Namespace() string { return c.namespace }

// do runs one API request and returns the response body, mapping any non-2xx
// status to an error carrying the server's message.
func (c *Client) do(ctx context.Context, method, path string, contentType string, body []byte) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("kubeops: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("kubeops: %s %s: read body: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("kubeops: %s %s: status %d: %s", method, path, resp.StatusCode, truncate(out, 300))
	}
	return out, nil
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "…"
	}
	return string(b)
}

// Pod is the slice of a pod the console renders.
type Pod struct {
	Name         string     `json:"name"`
	App          string     `json:"app"` // app label — maps a pod to its Deployment
	Phase        string     `json:"phase"`
	Ready        bool       `json:"ready"`
	RestartCount int        `json:"restart_count"`
	StartTime    *time.Time `json:"start_time,omitempty"`
	Image        string     `json:"image"`
}

// ListPods lists the namespace's pods with just the matrix fields.
func (c *Client) ListPods(ctx context.Context) ([]Pod, error) {
	body, err := c.do(ctx, http.MethodGet, "/api/v1/namespaces/"+c.namespace+"/pods", "", nil)
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Spec struct {
				Containers []struct {
					Image string `json:"image"`
				} `json:"containers"`
			} `json:"spec"`
			Status struct {
				Phase             string     `json:"phase"`
				StartTime         *time.Time `json:"startTime"`
				ContainerStatuses []struct {
					Ready        bool `json:"ready"`
					RestartCount int  `json:"restartCount"`
				} `json:"containerStatuses"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("kubeops: parse pod list: %w", err)
	}
	pods := make([]Pod, 0, len(list.Items))
	for _, it := range list.Items {
		p := Pod{
			Name:      it.Metadata.Name,
			App:       it.Metadata.Labels["app"],
			Phase:     it.Status.Phase,
			StartTime: it.Status.StartTime,
			Ready:     len(it.Status.ContainerStatuses) > 0,
		}
		if len(it.Spec.Containers) > 0 {
			p.Image = it.Spec.Containers[0].Image
		}
		for _, cs := range it.Status.ContainerStatuses {
			if !cs.Ready {
				p.Ready = false
			}
			p.RestartCount += cs.RestartCount
		}
		pods = append(pods, p)
	}
	return pods, nil
}

// RestartDeployment performs a rollout restart the way kubectl does: a
// strategic-merge patch setting the restartedAt pod-template annotation, which
// changes the template hash and rolls new pods.
func (c *Client) RestartDeployment(ctx context.Context, name string) error {
	patch, err := json.Marshal(map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"metadata": map[string]any{
					"annotations": map[string]string{
						"kubectl.kubernetes.io/restartedAt": c.now().Format(time.RFC3339),
					},
				},
			},
		},
	})
	if err != nil {
		return err
	}
	_, err = c.do(ctx, http.MethodPatch,
		"/apis/apps/v1/namespaces/"+c.namespace+"/deployments/"+url.PathEscape(name),
		"application/strategic-merge-patch+json", patch)
	return err
}

// PodLogs returns the last tailLines lines of a pod's logs as plain text.
// container may be empty for single-container pods.
func (c *Client) PodLogs(ctx context.Context, pod, container string, tailLines int) (string, error) {
	q := url.Values{}
	if tailLines > 0 {
		q.Set("tailLines", strconv.Itoa(tailLines))
	}
	if container != "" {
		q.Set("container", container)
	}
	body, err := c.do(ctx, http.MethodGet,
		"/api/v1/namespaces/"+c.namespace+"/pods/"+url.PathEscape(pod)+"/log?"+q.Encode(), "", nil)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// CronJob is the slice of a cronjob the console renders.
type CronJob struct {
	Name             string     `json:"name"`
	Schedule         string     `json:"schedule"`
	Suspend          bool       `json:"suspend"`
	LastScheduleTime *time.Time `json:"last_schedule_time,omitempty"`
}

// ListCronJobs lists the namespace's cronjobs.
func (c *Client) ListCronJobs(ctx context.Context) ([]CronJob, error) {
	body, err := c.do(ctx, http.MethodGet, "/apis/batch/v1/namespaces/"+c.namespace+"/cronjobs", "", nil)
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Schedule string `json:"schedule"`
				Suspend  bool   `json:"suspend"`
			} `json:"spec"`
			Status struct {
				LastScheduleTime *time.Time `json:"lastScheduleTime"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("kubeops: parse cronjob list: %w", err)
	}
	out := make([]CronJob, 0, len(list.Items))
	for _, it := range list.Items {
		out = append(out, CronJob{
			Name:             it.Metadata.Name,
			Schedule:         it.Spec.Schedule,
			Suspend:          it.Spec.Suspend,
			LastScheduleTime: it.Status.LastScheduleTime,
		})
	}
	return out, nil
}

// TriggerCronJob creates a Job from a cronjob's jobTemplate (what
// `kubectl create job --from=cronjob/x` does), named
// <cronjob>-manual-<unixts> and labelled ManualTriggerLabel so manual runs
// are identifiable. Returns the created Job name.
func (c *Client) TriggerCronJob(ctx context.Context, name string) (string, error) {
	body, err := c.do(ctx, http.MethodGet,
		"/apis/batch/v1/namespaces/"+c.namespace+"/cronjobs/"+url.PathEscape(name), "", nil)
	if err != nil {
		return "", err
	}
	// The jobTemplate spec is passed through verbatim — only the fields the
	// new Job's metadata needs are parsed.
	var cj struct {
		Spec struct {
			JobTemplate struct {
				Spec json.RawMessage `json:"spec"`
			} `json:"jobTemplate"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(body, &cj); err != nil {
		return "", fmt.Errorf("kubeops: parse cronjob %s: %w", name, err)
	}
	if len(cj.Spec.JobTemplate.Spec) == 0 {
		return "", fmt.Errorf("kubeops: cronjob %s has no jobTemplate spec", name)
	}
	jobName := fmt.Sprintf("%s-manual-%d", name, c.now().Unix())
	job, err := json.Marshal(map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata": map[string]any{
			"name":      jobName,
			"namespace": c.namespace,
			"labels":    map[string]string{ManualTriggerLabel: "true"},
		},
		"spec": cj.Spec.JobTemplate.Spec,
	})
	if err != nil {
		return "", err
	}
	if _, err := c.do(ctx, http.MethodPost,
		"/apis/batch/v1/namespaces/"+c.namespace+"/jobs", "application/json", job); err != nil {
		return "", err
	}
	return jobName, nil
}

// Job is the slice of a batch Job the console renders.
type Job struct {
	Name      string     `json:"name"`
	Succeeded int        `json:"succeeded"`
	Failed    int        `json:"failed"`
	Active    int        `json:"active"`
	StartTime *time.Time `json:"start_time,omitempty"`
}

// ListJobs lists the namespace's Jobs, optionally filtered by labelSelector
// (e.g. ManualTriggerLabel to see manual cron triggers only).
func (c *Client) ListJobs(ctx context.Context, labelSelector string) ([]Job, error) {
	path := "/apis/batch/v1/namespaces/" + c.namespace + "/jobs"
	if labelSelector != "" {
		path += "?labelSelector=" + url.QueryEscape(labelSelector)
	}
	body, err := c.do(ctx, http.MethodGet, path, "", nil)
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Status struct {
				Succeeded int        `json:"succeeded"`
				Failed    int        `json:"failed"`
				Active    int        `json:"active"`
				StartTime *time.Time `json:"startTime"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("kubeops: parse job list: %w", err)
	}
	out := make([]Job, 0, len(list.Items))
	for _, it := range list.Items {
		out = append(out, Job{
			Name:      it.Metadata.Name,
			Succeeded: it.Status.Succeeded,
			Failed:    it.Status.Failed,
			Active:    it.Status.Active,
			StartTime: it.Status.StartTime,
		})
	}
	return out, nil
}

// PVC is the slice of a PersistentVolumeClaim the console renders.
type PVC struct {
	Name      string `json:"name"`
	Phase     string `json:"phase"`
	Requested string `json:"requested"` // storage request, e.g. "10Gi"
}

// ListPVCs lists the namespace's persistent volume claims.
func (c *Client) ListPVCs(ctx context.Context) ([]PVC, error) {
	body, err := c.do(ctx, http.MethodGet, "/api/v1/namespaces/"+c.namespace+"/persistentvolumeclaims", "", nil)
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Resources struct {
					Requests map[string]string `json:"requests"`
				} `json:"resources"`
			} `json:"spec"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("kubeops: parse pvc list: %w", err)
	}
	out := make([]PVC, 0, len(list.Items))
	for _, it := range list.Items {
		out = append(out, PVC{
			Name:      it.Metadata.Name,
			Phase:     it.Status.Phase,
			Requested: it.Spec.Resources.Requests["storage"],
		})
	}
	return out, nil
}
