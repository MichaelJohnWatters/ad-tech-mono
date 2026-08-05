package objects

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects/fs"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects/s3"
)

// Connect returns the object store selected by the platform s3.* config keys:
// Minio/real S3 when s3.endpoint is set, else a local filesystem store rooted
// at fsRoot. Shared by every service that touches object storage so the
// selection logic lives once.
//
// When s3.endpoint IS set but the client fails to initialise, Connect used to
// latch the filesystem fallback for the life of the pod — the boot-latch
// disease (with 3 replicas, "filesystem" is three private emptyDirs =
// split-brain). Now it serves the fs fallback while a background loop keeps
// retrying the S3 dial and swaps the real backend in the moment it lands —
// the SelfHealingL2 posture.
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
	dial := func() (Store, error) {
		return s3.New(s3.Config{
			Endpoint:  endpoint,
			AccessKey: keys.S3.AccessKey.Get(cfg),
			SecretKey: keys.S3.SecretKey.Get(cfg),
			Region:    keys.S3.Region.Get(cfg),
			UseSSL:    keys.S3.UseSSL.Get(cfg),
		})
	}
	store, err := dial()
	if err == nil {
		log.Info("s3 connected", "endpoint", endpoint)
		return store
	}
	log.Error("s3 init failed; serving filesystem fallback and retrying in background", "endpoint", endpoint, "error", err)
	fsStore, _ := fs.New(fsRoot)
	sh := &selfHealingStore{}
	sh.current.Store(&storeHolder{s: fsStore})
	go func() {
		for {
			time.Sleep(10 * time.Second)
			s3Store, err := dial()
			if err != nil {
				continue
			}
			sh.current.Store(&storeHolder{s: s3Store})
			log.Info("s3 recovered; switched from filesystem fallback to the real backend "+
				"(fallback-era writes stay on the pod's local disk)", "endpoint", endpoint)
			return
		}
	}()
	return sh
}

// storeHolder wraps the interface so atomic.Pointer has a concrete type.
type storeHolder struct{ s Store }

// selfHealingStore delegates to whichever backend is current — the fs
// fallback until the background dial lands S3, then S3.
type selfHealingStore struct{ current atomic.Pointer[storeHolder] }

func (h *selfHealingStore) get() Store { return h.current.Load().s }

func (h *selfHealingStore) Put(ctx context.Context, bucket, key string, body io.Reader, size int64, contentType string) error {
	return h.get().Put(ctx, bucket, key, body, size, contentType)
}

func (h *selfHealingStore) Get(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	return h.get().Get(ctx, bucket, key)
}

func (h *selfHealingStore) Delete(ctx context.Context, bucket, key string) error {
	return h.get().Delete(ctx, bucket, key)
}

func (h *selfHealingStore) List(ctx context.Context, bucket, prefix string) ([]string, error) {
	return h.get().List(ctx, bucket, prefix)
}

func (h *selfHealingStore) Exists(ctx context.Context, bucket, key string) (bool, error) {
	return h.get().Exists(ctx, bucket, key)
}

func (h *selfHealingStore) PresignedGetURL(ctx context.Context, bucket, key string, ttl time.Duration) (string, error) {
	return h.get().PresignedGetURL(ctx, bucket, key, ttl)
}

func (h *selfHealingStore) EnsureBucket(ctx context.Context, bucket string) error {
	return h.get().EnsureBucket(ctx, bucket)
}

var _ Store = (*selfHealingStore)(nil)
