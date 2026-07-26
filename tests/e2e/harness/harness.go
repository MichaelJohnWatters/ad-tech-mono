//go:build e2e

// Package harness drives functionality tests against a running local stack.
//
// Tests assume `tilt up` (or any deployment exposing the standard
// ad-tech-mono ports) is already running. The harness:
//
//  1. Polls /readyz on every service before yielding to the test
//  2. Resets state (truncates Postgres, FLUSHDB Redis, clears Minio bucket)
//  3. Exposes helpers that mirror future API endpoints — when an HTTP/gRPC
//     API doesn't exist yet for an operation (e.g. admin signup), the helper
//     uses direct SQL. As real endpoints land the helper switches to HTTP
//     without changing the test.
//
// Build tag `e2e` keeps these out of `go test ./...` — run via `make test-e2e`.
package harness

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	_ "github.com/lib/pq"
)

// URLs of every locally-deployed service. Sourced from pkg/routes so port
// changes flow through automatically. Each service has two URLs:
//
//   - The bare name (DSP, Exchange, etc.) is reachable from the test process
//     on the host (via Tilt port-forward). Use for direct probes from tests.
//   - The Cluster* variant is reachable from inside the cluster (via k8s
//     Service DNS). Use when writing a value into a service's config that
//     another pod will dial — e.g. exchange.dsp_endpoints. The host's
//     localhost:8082 doesn't work from inside a pod (it's the pod's own
//     loopback), so cluster-internal DNS is required.
type URLs struct {
	Gateway           string
	Exchange          string
	DSP               string
	DSPComp1          string
	DSPComp2          string
	Tracker           string
	SSP               string
	AdServer          string
	Reporting         string
	PublisherAdServer string
	SSAI              string
	NATSURL           string
	RedisAddr         string
	MinioEndpt        string
	PostgresURL       string
	ClickHouseHTTP    string

	// Cluster-internal DNS variants, for config values that pods will
	// dial. Always populated; the test never has to choose between them.
	ClusterExchange string
	ClusterDSP      string
	ClusterDSPComp1 string
	ClusterDSPComp2 string
	ClusterTracker  string
	ClusterSSP      string
	ClusterAdServer string
}

// DefaultURLs returns the addresses exposed by the local Tilt stack.
//
// E2E_POSTGRES_URL overrides the Postgres DSN — the escape hatch for a stack
// whose localhost forward is wedged (post-sleep Rancher Desktop) but whose
// LoadBalancer service is still reachable on the VM IP, e.g.
// postgres://adtech:adtech-local-dev@192.168.64.2:5432/adtech?sslmode=disable.
func DefaultURLs() URLs {
	pgURL := routes.DefaultPostgresURL
	if v := os.Getenv("E2E_POSTGRES_URL"); v != "" {
		pgURL = v
	}
	return URLs{
		Gateway:           routes.DefaultGatewayURL,
		Exchange:          routes.DefaultExchangeURL,
		DSP:               routes.DefaultDSPURL,
		DSPComp1:          routes.DefaultDSPComp1URL,
		DSPComp2:          routes.DefaultDSPComp2URL,
		Tracker:           routes.DefaultTrackerURL,
		SSP:               routes.DefaultSSPURL,
		AdServer:          routes.DefaultAdServerURL,
		Reporting:         routes.DefaultReportingURL,
		PublisherAdServer: routes.DefaultPublisherAdServerURL,
		SSAI:              routes.DefaultSSAIURL,
		NATSURL:           routes.DefaultNATSURL,
		RedisAddr:         routes.DefaultRedisAddr,
		MinioEndpt:        routes.DefaultMinioEndpoint,
		PostgresURL:       pgURL,
		ClickHouseHTTP:    routes.DefaultClickHouseHTTPURL,

		// In-cluster DNS — matches the Service names in k8s/base/*/service.yaml.
		// Used when a test writes a config value that a pod will dial
		// (e.g. exchange.dsp_endpoints, publisher_adserver.prebid_servers).
		ClusterExchange: "http://exchange:" + routes.PortExchange,
		// ClusterDSP is the canonical exchange.dsp_endpoints ENTRY for our
		// DSP, not a plain base URL: the bid edge rides the internal gRPC
		// twin and ;notify= carries the HTTP base for OpenRTB win/loss
		// notices (budget caps depend on them). Tests that override
		// dsp_endpoints and restore with this value keep the deployed
		// transport instead of silently reverting the stack to HTTP.
		ClusterDSP: "grpc://dsp-internal-grpc:" + routes.PortDSPGRPC +
			";notify=http://dsp-internal:" + routes.PortDSP,
		ClusterDSPComp1: "http://dsp-competitor1:" + routes.PortDSPComp1,
		ClusterDSPComp2: "http://dsp-competitor2:" + routes.PortDSPComp2,
		ClusterTracker:  "http://tracker:" + routes.PortTracker,
		ClusterSSP:      "http://ssp:" + routes.PortSSP,
		ClusterAdServer: "http://adserver:" + routes.PortAdServer,
	}
}

// Harness is the shared test fixture. Construct once per test (typically via
// WaitReady in TestMain or at the top of a TestEndToEnd) and pass through
// subtests by value or via t.Helper accessors.
type Harness struct {
	URLs URLs
	DB   *sql.DB
	HTTP *http.Client

	adminTok string // cached admin bearer for auth-gated ops endpoints (config PUT)
}

// New connects to Postgres and returns a ready-to-use Harness. Does NOT
// call WaitReady — callers explicitly do that so they can choose to skip
// it (e.g. when iterating on a single test against an already-warm stack).
func New(t *testing.T) *Harness {
	t.Helper()
	urls := DefaultURLs()

	db, err := sql.Open("postgres", urls.PostgresURL)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping postgres: %v", err)
	}

	t.Cleanup(func() { db.Close() })

	return &Harness{
		URLs: urls,
		DB:   db,
		HTTP: &http.Client{Timeout: 10 * time.Second},
	}
}

// WithTenant runs fn inside a transaction with SET LOCAL app.current_account_id
// set to accountID — required for any INSERT/UPDATE on RLS-protected tables.
func (h *Harness) WithTenant(t *testing.T, accountID string, fn func(*sql.Tx)) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	// `SET LOCAL` doesn't accept parameter placeholders in Postgres, so we
	// use set_config(..., is_local=true) which is the parameterised form.
	// Same value semantics: scoped to this transaction only.
	if _, err := tx.ExecContext(ctx, "SELECT set_config('app.current_account_id', $1, true)", accountID); err != nil {
		tx.Rollback()
		t.Fatalf("set tenant: %v", err)
	}
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
			panic(r)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}()
	fn(tx)
}

// Fatalf is a convenience for harness internals to report errors via the
// active testing.T. We don't import t.Fatalf into helpers directly because
// helpers can be called from goroutines (e.g. NATS subscribers).
func Fatalf(t *testing.T, format string, args ...any) {
	t.Helper()
	t.Fatal(fmt.Sprintf(format, args...))
}
