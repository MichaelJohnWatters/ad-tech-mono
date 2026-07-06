// Package redis is the L2Cache implementation backed by Redis.
//
// Wraps go-redis to satisfy the cache.L2Cache interface so services
// can swap in MemoryL2 for tests without touching call sites.
package redis

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// Client is a Redis-backed L2Cache.
type Client struct {
	rdb *redis.Client
}

// Config holds connection settings.
type Config struct {
	Addr     string
	Password string
	DB       int
}

// New creates a Client and verifies the connection with PING.
func New(ctx context.Context, cfg Config) (*Client, error) {
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.Addr,
		Password: cfg.Password,
		DB:       cfg.DB,
	})
	if err := rdb.Ping(ctx).Err(); err != nil {
		return nil, err
	}
	return &Client{rdb: rdb}, nil
}

func (c *Client) Get(ctx context.Context, key string) (string, bool, error) {
	v, err := c.rdb.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

func (c *Client) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	return c.rdb.Set(ctx, key, value, ttl).Err()
}

func (c *Client) Delete(ctx context.Context, key string) error {
	return c.rdb.Del(ctx, key).Err()
}

func (c *Client) SetNX(ctx context.Context, key, value string, ttl time.Duration) (bool, error) {
	return c.rdb.SetNX(ctx, key, value, ttl).Result()
}

func (c *Client) Incr(ctx context.Context, key string) (int64, error) {
	return c.rdb.Incr(ctx, key).Result()
}

func (c *Client) IncrBy(ctx context.Context, key string, n int64) (int64, error) {
	return c.rdb.IncrBy(ctx, key, n).Result()
}

func (c *Client) DecrBy(ctx context.Context, key string, n int64) (int64, error) {
	return c.rdb.DecrBy(ctx, key, n).Result()
}

func (c *Client) Expire(ctx context.Context, key string, ttl time.Duration) error {
	return c.rdb.Expire(ctx, key, ttl).Err()
}

// Set operations — used by the identity-consumer's Redis-backed fingerprint
// buckets so probabilistic matching stays coherent across multiple replicas.

func (c *Client) SAdd(ctx context.Context, key, member string) error {
	return c.rdb.SAdd(ctx, key, member).Err()
}

func (c *Client) SCard(ctx context.Context, key string) (int64, error) {
	return c.rdb.SCard(ctx, key).Result()
}

func (c *Client) SMembers(ctx context.Context, key string) ([]string, error) {
	return c.rdb.SMembers(ctx, key).Result()
}

func (c *Client) SIsMember(ctx context.Context, key, member string) (bool, error) {
	return c.rdb.SIsMember(ctx, key, member).Result()
}

func (c *Client) Ping(ctx context.Context) error {
	return c.rdb.Ping(ctx).Err()
}

func (c *Client) Close() error {
	return c.rdb.Close()
}
