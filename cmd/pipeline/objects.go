package main

import (
	"log/slog"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects/fs"
	objs3 "github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects/s3"
)

// connectObjects returns the object store the data lake writes Parquet to:
// Minio/S3 when s3.endpoint is set, else a local filesystem fallback so the
// pipeline still runs offline (matches the ad server's pattern).
func connectObjects(cfg *config.Config, log *slog.Logger) objects.Store {
	const fsRoot = "/tmp/adtech-datalake"
	endpoint := cfg.Get("s3.endpoint", "")
	if endpoint == "" {
		log.Warn("s3.endpoint not set, datalake using local filesystem", "root", fsRoot)
		store, err := fs.New(fsRoot)
		if err != nil {
			log.Error("datalake fs store init failed", "error", err)
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
		log.Error("datalake s3 init failed, falling back to filesystem", "error", err)
		fsStore, _ := fs.New(fsRoot)
		return fsStore
	}
	log.Info("datalake object store connected", "endpoint", endpoint)
	return store
}
