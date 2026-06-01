// Package objects is the object storage abstraction.
//
// Store is implemented by S3/Minio (production, staging, local k3s)
// and by FS (filesystem, used in unit tests). All services read and
// write blobs (creatives, parquet shards, exports) through this interface.
package objects

import (
	"context"
	"io"
	"time"
)

// Store reads and writes opaque byte streams keyed by bucket+key.
type Store interface {
	// Put writes data to bucket/key. contentType is optional (e.g. "text/html").
	Put(ctx context.Context, bucket, key string, body io.Reader, size int64, contentType string) error

	// Get returns the object body. Caller must Close.
	Get(ctx context.Context, bucket, key string) (io.ReadCloser, error)

	// Delete removes the object. No-op if missing.
	Delete(ctx context.Context, bucket, key string) error

	// List returns keys with the given prefix.
	List(ctx context.Context, bucket, prefix string) ([]string, error)

	// Exists reports whether the object exists.
	Exists(ctx context.Context, bucket, key string) (bool, error)

	// PresignedGetURL returns a time-limited URL the browser can use to fetch.
	PresignedGetURL(ctx context.Context, bucket, key string, ttl time.Duration) (string, error)

	// EnsureBucket creates the bucket if missing.
	EnsureBucket(ctx context.Context, bucket string) error
}
