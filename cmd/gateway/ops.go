package main

// ops.go — the staff ops console API (/v1/api/ops/*): monitor AND act on the
// k8s stack from the staff portal. Reads (pod matrix, per-service readyz
// fan-out, NATS lag, cronjobs, jobs, PVCs, log tails) are gated ops:read;
// mutations (rollout restart, cronjob trigger) are gated ops:deploy and every
// one writes an audit entry. Backed by pkg/kubeops' in-cluster client — a nil
// client (gateway running off-cluster) degrades to 503 with an ERROR log, per
// the "failures must be ERROR logs" convention.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/audit"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/kubeops"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// k8sNameRe is the DNS-label shape every pod/deployment/cronjob name has —
// the gate that keeps request params from turning into arbitrary API paths.
var k8sNameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// opsTarget is one service in the readyz grid: name + in-cluster base URL.
type opsTarget struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// opsAuditFn records one ops mutation. Split out (rather than calling
// audit.Log inline) so handler tests can capture entries without a database;
// the production fn wraps audit.Log with the gateway's DB handle.
type opsAuditFn func(ctx context.Context, e audit.Entry)

// opsAPI bundles the ops-console dependencies. kube may be nil (off-cluster).
type opsAPI struct {
	kube           *kubeops.Client
	targets        []opsTarget
	natsMonitorURL string
	audit          opsAuditFn
	log            *slog.Logger
	// probe is the short-timeout client for readyz fan-out + NATS monitor.
	probe *http.Client
}

func newOpsAPI(kube *kubeops.Client, targets []opsTarget, natsMonitorURL string, auditFn opsAuditFn, log *slog.Logger) *opsAPI {
	return &opsAPI{
		kube:           kube,
		targets:        targets,
		natsMonitorURL: natsMonitorURL,
		audit:          auditFn,
		log:            log,
		probe:          &http.Client{Timeout: 2 * time.Second},
	}
}

// natsMonitorURLFrom derives the NATS HTTP monitoring endpoint from the
// client URL (nats://host:4222 → http://host:8222). One config key, one host.
func natsMonitorURLFrom(natsURL string) string {
	u, err := url.Parse(natsURL)
	if err != nil || u.Host == "" {
		return ""
	}
	host := u.Host
	if h, _, err := net.SplitHostPort(u.Host); err == nil {
		host = h
	}
	return "http://" + host + ":" + routes.PortNATSMonitor
}

// kubeReady 503s (with the required ERROR log) when the in-cluster client is
// absent — every k8s-backed handler's first gate.
func (o *opsAPI) kubeReady(w http.ResponseWriter) bool {
	if o.kube == nil {
		o.log.Error("ops: kubernetes client unavailable — gateway is not running in-cluster (no serviceaccount mount)")
		http.Error(w, `{"error":"kubernetes unavailable"}`, http.StatusServiceUnavailable)
		return false
	}
	return true
}

func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method != method {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return false
	}
	return true
}

// ---- Reads (ops:read) ----

func (o *opsAPI) podsHandler(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) || !o.kubeReady(w) {
		return
	}
	pods, err := o.kube.ListPods(r.Context())
	if err != nil {
		o.log.Error("ops: list pods failed", "error", err)
		http.Error(w, `{"error":"kubernetes unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"namespace": o.kube.Namespace(), "pods": pods})
}

// readyzView is one service's probe outcome in the grid.
type readyzView struct {
	Service   string `json:"service"`
	URL       string `json:"url"`
	OK        bool   `json:"ok"`
	Status    int    `json:"status,omitempty"`
	Body      string `json:"body,omitempty"`
	LatencyMS int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

func (o *opsAPI) readyzGridHandler(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	out := make([]readyzView, len(o.targets))
	var wg sync.WaitGroup
	for i, t := range o.targets {
		wg.Add(1)
		go func(i int, t opsTarget) {
			defer wg.Done()
			v := readyzView{Service: t.Name, URL: t.URL}
			start := time.Now()
			req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, t.URL+routes.Readyz, nil)
			if err != nil {
				v.Error = err.Error()
				out[i] = v
				return
			}
			resp, err := o.probe.Do(req)
			v.LatencyMS = time.Since(start).Milliseconds()
			if err != nil {
				v.Error = err.Error()
				out[i] = v
				return
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
			v.Status = resp.StatusCode
			v.Body = string(body)
			v.OK = resp.StatusCode == http.StatusOK
			out[i] = v
		}(i, t)
	}
	wg.Wait()
	for _, v := range out {
		if !v.OK {
			o.log.Error("ops: service not ready", "service", v.Service, "status", v.Status, "error", v.Error)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"services": out})
}

// natsStreamView / natsConsumerView summarise /jsz for the lag card.
type natsStreamView struct {
	Name      string `json:"name"`
	Messages  uint64 `json:"messages"`
	Bytes     uint64 `json:"bytes"`
	Consumers int    `json:"consumers"`
}

type natsConsumerView struct {
	Stream        string `json:"stream"`
	Name          string `json:"name"`
	NumPending    uint64 `json:"num_pending"`
	NumAckPending int    `json:"num_ack_pending"`
	NumRedeliv    int    `json:"num_redelivered"`
}

func (o *opsAPI) natsHandler(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet,
		o.natsMonitorURL+"/jsz?streams=true&consumers=true&accounts=true", nil)
	if err != nil {
		http.Error(w, `{"error":"nats monitor unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	resp, err := o.probe.Do(req)
	if err != nil {
		o.log.Error("ops: nats monitor unreachable", "url", o.natsMonitorURL, "error", err)
		http.Error(w, `{"error":"nats monitor unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		o.log.Error("ops: nats monitor /jsz failed", "status", resp.StatusCode)
		http.Error(w, `{"error":"nats monitor unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	var jsz struct {
		Streams        int    `json:"streams"`
		Consumers      int    `json:"consumers"`
		Messages       uint64 `json:"messages"`
		Bytes          uint64 `json:"bytes"`
		AccountDetails []struct {
			StreamDetail []struct {
				Name  string `json:"name"`
				State struct {
					Messages      uint64 `json:"messages"`
					Bytes         uint64 `json:"bytes"`
					ConsumerCount int    `json:"consumer_count"`
				} `json:"state"`
				Consumers []struct {
					Name           string `json:"name"`
					NumPending     uint64 `json:"num_pending"`
					NumAckPending  int    `json:"num_ack_pending"`
					NumRedelivered int    `json:"num_redelivered"`
				} `json:"consumer_detail"`
			} `json:"stream_detail"`
		} `json:"account_details"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&jsz); err != nil {
		o.log.Error("ops: nats /jsz parse failed", "error", err)
		http.Error(w, `{"error":"nats monitor unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	streams := []natsStreamView{}
	lagging := []natsConsumerView{}
	for _, acct := range jsz.AccountDetails {
		for _, s := range acct.StreamDetail {
			streams = append(streams, natsStreamView{
				Name: s.Name, Messages: s.State.Messages, Bytes: s.State.Bytes, Consumers: s.State.ConsumerCount,
			})
			for _, c := range s.Consumers {
				if c.NumPending > 0 || c.NumAckPending > 0 {
					lagging = append(lagging, natsConsumerView{
						Stream: s.Name, Name: c.Name, NumPending: c.NumPending,
						NumAckPending: c.NumAckPending, NumRedeliv: c.NumRedelivered,
					})
				}
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"streams": streams, "lagging": lagging,
		"totals": map[string]any{"streams": jsz.Streams, "consumers": jsz.Consumers, "messages": jsz.Messages, "bytes": jsz.Bytes},
	})
}

func (o *opsAPI) cronJobsHandler(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) || !o.kubeReady(w) {
		return
	}
	cjs, err := o.kube.ListCronJobs(r.Context())
	if err != nil {
		o.log.Error("ops: list cronjobs failed", "error", err)
		http.Error(w, `{"error":"kubernetes unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"cronjobs": cjs})
}

func (o *opsAPI) jobsHandler(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) || !o.kubeReady(w) {
		return
	}
	// Default to manual-trigger jobs — the ones this console creates.
	sel := r.URL.Query().Get("label_selector")
	if sel == "" {
		sel = kubeops.ManualTriggerLabel
	}
	jobs, err := o.kube.ListJobs(r.Context(), sel)
	if err != nil {
		o.log.Error("ops: list jobs failed", "error", err)
		http.Error(w, `{"error":"kubernetes unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jobs": jobs})
}

func (o *opsAPI) pvcsHandler(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) || !o.kubeReady(w) {
		return
	}
	pvcs, err := o.kube.ListPVCs(r.Context())
	if err != nil {
		o.log.Error("ops: list pvcs failed", "error", err)
		http.Error(w, `{"error":"kubernetes unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"pvcs": pvcs})
}

func (o *opsAPI) logsHandler(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) || !o.kubeReady(w) {
		return
	}
	pod := r.URL.Query().Get("pod")
	if !k8sNameRe.MatchString(pod) {
		http.Error(w, `{"error":"pod query param must be a k8s pod name"}`, http.StatusBadRequest)
		return
	}
	container := r.URL.Query().Get("container")
	if container != "" && !k8sNameRe.MatchString(container) {
		http.Error(w, `{"error":"invalid container name"}`, http.StatusBadRequest)
		return
	}
	tail := 200
	if t := r.URL.Query().Get("tail"); t != "" {
		n, err := strconv.Atoi(t)
		if err != nil || n < 1 || n > 5000 {
			http.Error(w, `{"error":"tail must be 1-5000"}`, http.StatusBadRequest)
			return
		}
		tail = n
	}
	logs, err := o.kube.PodLogs(r.Context(), pod, container, tail)
	if err != nil {
		o.log.Error("ops: pod logs failed", "pod", pod, "error", err)
		http.Error(w, `{"error":"kubernetes unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(logs))
}

// ---- Mutations (ops:deploy — audit-logged) ----

func (o *opsAPI) restartHandler(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) || !o.kubeReady(w) {
		return
	}
	var req struct {
		Deployment string `json:"deployment"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !k8sNameRe.MatchString(req.Deployment) {
		http.Error(w, `{"error":"body must be {\"deployment\":\"<name>\"}"}`, http.StatusBadRequest)
		return
	}
	if err := o.kube.RestartDeployment(r.Context(), req.Deployment); err != nil {
		o.log.Error("ops: rollout restart failed", "deployment", req.Deployment, "error", err)
		http.Error(w, `{"error":"restart failed"}`, http.StatusServiceUnavailable)
		return
	}
	claims := middleware.ClaimsFromContext(r.Context())
	o.audit(r.Context(), audit.Entry{
		ActorID:      claims.UserID,
		ActorIP:      r.RemoteAddr,
		Action:       "ops:restart_deployment",
		ResourceType: "deployment",
		ResourceID:   req.Deployment,
	})
	o.log.Info("ops: rollout restart requested", "deployment", req.Deployment, "actor", claims.UserID)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"deployment": req.Deployment, "restarted": true})
}

func (o *opsAPI) cronJobTriggerHandler(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) || !o.kubeReady(w) {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !k8sNameRe.MatchString(req.Name) {
		http.Error(w, `{"error":"body must be {\"name\":\"<cronjob>\"}"}`, http.StatusBadRequest)
		return
	}
	jobName, err := o.kube.TriggerCronJob(r.Context(), req.Name)
	if err != nil {
		o.log.Error("ops: cronjob trigger failed", "cronjob", req.Name, "error", err)
		http.Error(w, `{"error":"trigger failed"}`, http.StatusServiceUnavailable)
		return
	}
	claims := middleware.ClaimsFromContext(r.Context())
	o.audit(r.Context(), audit.Entry{
		ActorID:      claims.UserID,
		ActorIP:      r.RemoteAddr,
		Action:       "ops:trigger_cronjob",
		ResourceType: "cronjob",
		ResourceID:   req.Name,
		Changes:      map[string]string{"job": jobName},
	})
	o.log.Info("ops: cronjob triggered", "cronjob", req.Name, "job", jobName, "actor", claims.UserID)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"cronjob": req.Name, "job": jobName})
}
