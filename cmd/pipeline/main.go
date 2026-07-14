// cmd/pipeline is the data pipeline service.
// Ingests publisher data files, validates, normalises, and enriches them.
// Watches object storage (Minio/S3) for new files.
package main

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/health"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

func main() {
	log := logger.New(constants.ServicePipeline)
	sc := config.Setup(constants.ServicePipeline, nil, log)
	cfg := sc.Cfg
	_ = sc
	hlth := health.New()
	lc := lifecycle.New(log)

	port := keys.Pipeline.Port.Get(cfg)

	// Data-lake batch layer: consume the event stream (own NATS group, so it
	// runs alongside reporting's real-time consumer) and land events as
	// Parquet in object storage. Off only if pipeline.datalake_enabled=false.
	var sink *datalakeSink
	if keys.Pipeline.DatalakeEnabled.Get(cfg) {
		sink = startDatalakeSink(cfg, log, lc)
	}

	mux := http.NewServeMux()
	mux.Handle(routes.Healthz, hlth.LivenessHandler())
	mux.Handle(routes.Readyz, hlth.ReadinessHandler())

	// Cold-archive verification: report the Parquet snapshot (active files +
	// total rows/bytes from the Delta log) for a table, or all tables. Used by
	// ops and e2e to confirm the log→Parquet spine landed every event — the
	// Parquet TotalRows should reconcile with the reporting/NATS event count.
	if sink != nil && keys.Debug.EndpointsEnabled.Get(cfg) {
		mux.HandleFunc("/debug/datalake/snapshot", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
			ctx := r.Context()
			tables := sink.Tables()
			if t := r.URL.Query().Get("table"); t != "" {
				tables = []string{t}
			}
			out := map[string]any{}
			for _, t := range tables {
				snap, err := sink.Snapshot(ctx, t)
				if err != nil {
					out[t] = map[string]any{"error": err.Error()}
					continue
				}
				out[t] = map[string]any{
					"version": snap.Version, "total_rows": snap.TotalRows,
					"total_bytes": snap.TotalBytes, "active_files": len(snap.ActiveFiles),
				}
			}
			_ = json.NewEncoder(w).Encode(out)
		})
	}

	server := &http.Server{Addr: ":" + port, Handler: mux, ReadTimeout: 5 * time.Second, WriteTimeout: 30 * time.Second}

	log.Info("pipeline starting", "port", port)
	lifecycle.ServeHTTP(lc, server, log, 30*time.Second)
}
