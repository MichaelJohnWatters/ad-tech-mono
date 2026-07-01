// cmd/compact is the datalake compaction job (K8s CronJob). It bin-packs each
// event table's many small minutely Parquet files into a single consolidated
// file and marks the old ones removed in the Delta log — fixing the small-files
// problem that frequent flushes create. Row content is unchanged; a Read before
// and after returns the same records. Idempotent, so a re-run is safe.
//
// Run as a one-shot job (exits after compacting), scheduled hourly.
package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/datalake"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects/fs"
	objs3 "github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects/s3"
)

// tables are the datalake tables the pipeline writes (mirrors eventTables in
// cmd/pipeline). Kept as a literal list so the job has no NATS dependency.
var tables = []string{"impressions", "clicks", "conversions", "views", "auction_wins"}

func main() {
	log := logger.New(constants.ServicePipeline)
	sc := config.Setup(constants.ServicePipeline, nil, log)
	cfg := sc.Cfg

	bucket := cfg.Get("pipeline.datalake_bucket", "adtech-datalake")
	obj := connectObjects(cfg, log)
	if obj == nil {
		log.Error("compact: no object store, nothing to do")
		os.Exit(1)
	}
	lake := datalake.NewObjectStore(obj, bucket, log)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	var failed bool
	for _, table := range tables {
		res, err := lake.Compact(ctx, table)
		if err != nil {
			// A table that was never written has no log — skip, don't fail.
			log.Warn("compact: table skipped", "table", table, "error", err)
			continue
		}
		if res.FilesBefore > res.FilesAfter {
			log.Info("compact: table compacted", "table", table,
				"files_before", res.FilesBefore, "files_after", res.FilesAfter, "rows", res.Rows)
		} else {
			log.Info("compact: nothing to do", "table", table, "files", res.FilesAfter)
		}
	}
	if failed {
		os.Exit(1)
	}
	log.Info("compact: done", "tables", len(tables))
}

// connectObjects mirrors cmd/pipeline: Minio/S3 when s3.endpoint is set, else a
// local filesystem fallback so the job runs offline.
func connectObjects(cfg *config.Config, log *slog.Logger) objects.Store {
	const fsRoot = "/tmp/adtech-datalake"
	endpoint := cfg.Get("s3.endpoint", "")
	if endpoint == "" {
		log.Warn("s3.endpoint not set, compaction using local filesystem", "root", fsRoot)
		store, err := fs.New(fsRoot)
		if err != nil {
			log.Error("compact fs store init failed", "error", err)
			return nil
		}
		return store
	}
	store, err := objs3.New(objs3.Config{
		Endpoint:  endpoint,
		AccessKey: cfg.Get("s3.access_key", "adtech"),
		SecretKey: cfg.Get("s3.secret_key", "adtech-local-dev"),
		Region:    cfg.Get("s3.region", "us-east-1"),
		UseSSL:    cfg.GetBool("s3.use_ssl", false),
	})
	if err != nil {
		log.Error("compact s3 init failed", "error", err)
		return nil
	}
	log.Info("compact object store connected", "endpoint", endpoint)
	return store
}
