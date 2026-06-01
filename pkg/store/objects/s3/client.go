// Package s3 implements objects.Store using the Minio Go client.
//
// One code path serves both Minio (local k3s, staging) and real S3
// (production) — the endpoint, region, and credentials vary by env.
package s3

import (
	"context"
	"errors"
	"io"
	"net/url"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Config holds S3 connection settings.
type Config struct {
	Endpoint  string // host:port, e.g. "minio:9000" or "s3.amazonaws.com"
	AccessKey string
	SecretKey string
	Region    string
	UseSSL    bool
}

// Client is an objects.Store backed by S3/Minio.
type Client struct {
	mc *minio.Client
}

// New constructs a Client. Does not pre-create buckets — call EnsureBucket.
func New(cfg Config) (*Client, error) {
	mc, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, err
	}
	return &Client{mc: mc}, nil
}

func (c *Client) Put(ctx context.Context, bucket, key string, body io.Reader, size int64, contentType string) error {
	_, err := c.mc.PutObject(ctx, bucket, key, body, size, minio.PutObjectOptions{ContentType: contentType})
	return err
}

func (c *Client) Get(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	obj, err := c.mc.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	if _, err := obj.Stat(); err != nil {
		obj.Close()
		return nil, err
	}
	return obj, nil
}

func (c *Client) Delete(ctx context.Context, bucket, key string) error {
	return c.mc.RemoveObject(ctx, bucket, key, minio.RemoveObjectOptions{})
}

func (c *Client) List(ctx context.Context, bucket, prefix string) ([]string, error) {
	var keys []string
	for obj := range c.mc.ListObjects(ctx, bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if obj.Err != nil {
			return nil, obj.Err
		}
		keys = append(keys, obj.Key)
	}
	return keys, nil
}

func (c *Client) Exists(ctx context.Context, bucket, key string) (bool, error) {
	_, err := c.mc.StatObject(ctx, bucket, key, minio.StatObjectOptions{})
	if err == nil {
		return true, nil
	}
	var resp minio.ErrorResponse
	if errors.As(err, &resp) && resp.Code == "NoSuchKey" {
		return false, nil
	}
	return false, err
}

func (c *Client) PresignedGetURL(ctx context.Context, bucket, key string, ttl time.Duration) (string, error) {
	u, err := c.mc.PresignedGetObject(ctx, bucket, key, ttl, url.Values{})
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

func (c *Client) EnsureBucket(ctx context.Context, bucket string) error {
	ok, err := c.mc.BucketExists(ctx, bucket)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	return c.mc.MakeBucket(ctx, bucket, minio.MakeBucketOptions{})
}
