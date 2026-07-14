package objects

import (
	"log/slog"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects/fs"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects/s3"
)

// Connect returns the object store selected by the platform s3.* config keys:
// Minio/real S3 when s3.endpoint is set, else a local filesystem store rooted
// at fsRoot (and the same fallback if the S3 client fails to initialise).
// Shared by every service that touches object storage so the selection logic
// lives once.
func Connect(cfg *config.Config, fsRoot string, log *slog.Logger) Store {
	endpoint := cfg.Get(keys.S3.Endpoint.Key(), "")
	if endpoint == "" {
		log.Warn("s3.endpoint not set, using local filesystem", "root", fsRoot)
		store, err := fs.New(fsRoot)
		if err != nil {
			log.Error("fs store init failed", "error", err)
		}
		return store
	}
	store, err := s3.New(s3.Config{
		Endpoint:  endpoint,
		AccessKey: keys.S3.AccessKey.Get(cfg),
		SecretKey: keys.S3.SecretKey.Get(cfg),
		Region:    keys.S3.Region.Get(cfg),
		UseSSL:    keys.S3.UseSSL.Get(cfg),
	})
	if err != nil {
		log.Error("s3 init failed, falling back to filesystem", "error", err)
		fsStore, _ := fs.New(fsRoot)
		return fsStore
	}
	log.Info("s3 connected", "endpoint", endpoint)
	return store
}
