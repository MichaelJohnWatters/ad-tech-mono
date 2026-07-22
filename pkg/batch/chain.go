package batch

// chain.go — the platform's standard data chain, assembled from the
// existing job seams. Order IS the dependency graph:
//
//	1. checkpoint        (CRITICAL) — ingestion layer up? pipeline+reporting /readyz
//	2. compact → vacuum             — pipeline POST /v1/datalake/{compact,vacuum}
//	                                  (in the pipeline's process: single-writer rule;
//	                                  vacuum reclaims tombstoned bytes — the GDPR tail)
//	3. rollup:minute…monthly        — reporting /debug/rollup/run?level=X, finest first
//	4. profile-builder              — pkg/profilebuilder.Run in-process
//	5. privacy-delete    → verify   — pkg/privacydelete in-process, verify AFTER delete
//
// Every step is an idempotent wholesale-recompute, so non-critical failures
// continue the chain (stale, not wrong); only the checkpoint aborts it —
// if ingestion is down there is nothing meaningful to compute.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/privacydelete"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/profilebuilder"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/datalake"
)

// Deps wires the standard chain. DB is required; Lake/Bus optional with the
// same degradation semantics as the underlying jobs.
type Deps struct {
	DB           *sql.DB
	Lake         datalake.Store // profile-builder reads/writes
	Bus          events.EventBus
	PipelineURL  string
	ReportingURL string
	HTTP         *http.Client
	Log          *slog.Logger

	// Profile-builder knobs (zero = its defaults).
	MinConfidence  float64
	MaxClusterSize int

	// Behaviour repoints the profile-builder step's three lake reads
	// (behavioural rules, lookalike category signals, reconcile) onto
	// server-side ClickHouse GROUP BY (ADR 0006 phase 2). Nil = the builder
	// falls back to the lake reads.
	Behaviour profilebuilder.BehaviourQuerier

	// PrivacyExtras purge non-Postgres systems (lake via pipeline,
	// freq_cap_blocks via ClickHouse) — privacydelete.BuildExtras output.
	PrivacyExtras []privacydelete.ExtraPurger
}

func (d Deps) client() *http.Client {
	if d.HTTP != nil {
		return d.HTTP
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

// StandardChain builds the ordered step list from Deps.
func StandardChain(d Deps) []Step {
	steps := []Step{
		{
			// Approximates "ingestion caught up" via service READINESS, not
			// NATS consumer lag (the bus doesn't expose per-consumer lag).
			// Good enough because every downstream step is an idempotent
			// wholesale-recompute: events that land mid-chain are simply
			// picked up by the next run. Revisit if a step ever becomes
			// lag-sensitive.
			Name:     "checkpoint",
			Critical: true,
			Run: func(ctx context.Context) (string, error) {
				for _, svc := range []struct{ name, url string }{
					{"pipeline", d.PipelineURL + routes.Readyz},
					{"reporting", d.ReportingURL + routes.Readyz},
				} {
					if err := d.getOK(ctx, svc.url); err != nil {
						return "", fmt.Errorf("%s not ready: %w", svc.name, err)
					}
				}
				return "pipeline + reporting ready", nil
			},
		},
		{
			Name: "compact",
			Run: func(ctx context.Context) (string, error) {
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.PipelineURL+routes.DatalakeCompact, nil)
				if err != nil {
					return "", err
				}
				body, err := d.do(req)
				if err != nil {
					return "", err
				}
				var results map[string]datalake.CompactResult
				if err := json.Unmarshal(body, &results); err != nil {
					return "", fmt.Errorf("decode compact results: %w", err)
				}
				packed, files := 0, 0
				for _, r := range results {
					if r.FilesBefore > r.FilesAfter {
						packed++
						files += r.FilesBefore - r.FilesAfter
					}
				}
				return fmt.Sprintf("%d/%d tables packed (%d files removed)", packed, len(results), files), nil
			},
		},
		{
			Name: "vacuum",
			Run: func(ctx context.Context) (string, error) {
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.PipelineURL+routes.DatalakeVacuum, nil)
				if err != nil {
					return "", err
				}
				body, err := d.do(req)
				if err != nil {
					return "", err
				}
				var results map[string]datalake.VacuumResult
				if err := json.Unmarshal(body, &results); err != nil {
					return "", fmt.Errorf("decode vacuum results: %w", err)
				}
				files, bytes := 0, int64(0)
				for _, r := range results {
					files += r.FilesDeleted
					bytes += r.BytesFreed
				}
				return fmt.Sprintf("%d tombstoned files deleted (%d bytes freed)", files, bytes), nil
			},
		},
	}

	// Rollup tiers, finest first — each tier's inputs are fresher because
	// the previous one just ran, which is the ordering the cron offsets
	// only ever approximated. The minute tier looks back the whole hour the
	// chain covers (an hourly caller running only the last completed minute
	// would sample 1/60th of the tier); coarser tiers recompute their last
	// completed window, which the hourly cadence already covers.
	for _, tier := range []struct {
		level    string
		lookback string
	}{{"minute", "60"}, {"hourly", "1"}, {"daily", "1"}, {"monthly", "1"}} {
		level := tier.level
		steps = append(steps, Step{
			Name: "rollup:" + level,
			Run: func(ctx context.Context) (string, error) {
				req, err := http.NewRequestWithContext(ctx, http.MethodGet,
					d.ReportingURL+routes.ReportingRollupRun+"?level="+level+"&lookback="+tier.lookback, nil)
				if err != nil {
					return "", err
				}
				body, err := d.do(req)
				if err != nil {
					return "", err
				}
				// The endpoint returns the engine's per-config result ARRAY.
				var results []json.RawMessage
				if err := json.Unmarshal(body, &results); err != nil {
					return "", fmt.Errorf("decode rollup results: %w", err)
				}
				return fmt.Sprintf("%d rollup configs ran", len(results)), nil
			},
		})
	}

	// ADR 0006 phase 4: derive the Parquet lake from ClickHouse. Runs after
	// the rollups so the hour's raw rows are settled; idempotent per hour, so
	// a retried chain re-exports the same hour harmlessly. Exports the previous
	// full hour (the endpoint's default).
	steps = append(steps, Step{
		Name: "ch-parquet-export",
		Run: func(ctx context.Context) (string, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.ReportingURL+routes.ReportingExportRun, nil)
			if err != nil {
				return "", err
			}
			body, err := d.do(req)
			if err != nil {
				return "", err
			}
			var res struct {
				Rows    int64            `json:"rows"`
				Tables  map[string]int64 `json:"tables"`
				Skipped string           `json:"skipped"`
			}
			if err := json.Unmarshal(body, &res); err != nil {
				return "", fmt.Errorf("decode export results: %w", err)
			}
			if res.Skipped != "" {
				return res.Skipped, nil
			}
			return fmt.Sprintf("%d rows across %d tables exported to Parquet", res.Rows, len(res.Tables)), nil
		},
	})

	steps = append(steps,
		Step{
			Name: "profile-builder",
			Run: func(ctx context.Context) (string, error) {
				res, err := profilebuilder.Run(ctx, profilebuilder.Config{
					DB: d.DB, Lake: d.Lake, Bus: d.Bus, Log: d.Log,
					Behaviour:     d.Behaviour,
					MinConfidence: d.MinConfidence, MaxClusterSize: d.MaxClusterSize,
				})
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("clusters=%d enrolled=%d pruned=%d expanded=%d reconciled=%d",
					res.Clusters, res.Enrolled, res.Pruned, res.Expanded, res.Reconciled), nil
			},
		},
		Step{
			Name: "privacy-delete",
			Run: func(ctx context.Context) (string, error) {
				deleter := &privacydelete.Deleter{
					Store:    privacydelete.NewPostgresStore(d.DB).WithExtras(d.PrivacyExtras...),
					Announce: d.Bus,
					Subject:  events.SubjectPrivacyCompleted,
					Log:      d.Log,
				}
				n, err := deleter.RunPending(ctx)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("%d users deleted", n), nil
			},
		},
		Step{
			Name: "privacy-verify",
			Run: func(ctx context.Context) (string, error) {
				verifier := &privacydelete.Verifier{
					Store: privacydelete.NewPostgresStore(d.DB).WithExtras(d.PrivacyExtras...),
					Log:   d.Log,
				}
				verified, incomplete, err := verifier.RunUnverified(ctx)
				if err != nil {
					return "", err
				}
				if incomplete > 0 {
					// Residual PII after a deletion is an operational alert,
					// not a shrug — fail the step so the run shows red.
					return "", fmt.Errorf("%d deletions incomplete (residual data); %d verified", incomplete, verified)
				}
				return fmt.Sprintf("%d verified, 0 incomplete", verified), nil
			},
		},
	)
	return steps
}

func (d Deps) getOK(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := d.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %s", resp.Status)
	}
	return nil
}

func (d Deps) do(req *http.Request) ([]byte, error) {
	resp, err := d.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s %s: %s: %s", req.Method, req.URL.Path, resp.Status, string(body))
	}
	return body, nil
}
