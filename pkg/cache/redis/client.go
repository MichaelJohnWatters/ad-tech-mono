// Package redis is the L2Cache implementation backed by Redis.
//
// Wraps go-redis to satisfy the cache.L2Cache interface so services
// can swap in MemoryL2 for tests without touching call sites.
package redis

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Client is a Redis-backed L2Cache.
type Client struct {
	rdb *redis.Client
	// scripts caches redis.Script objects by body so Eval runs EVALSHA
	// after the first call instead of resending the script text.
	scripts sync.Map // script body -> *redis.Script
}

// Eval implements cache.Scripter with EVALSHA caching.
func (c *Client) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	v, ok := c.scripts.Load(script)
	if !ok {
		v, _ = c.scripts.LoadOrStore(script, redis.NewScript(script))
	}
	return v.(*redis.Script).Run(ctx, c.rdb, keys, args...).Result()
}

// Config holds connection settings.
type Config struct {
	Addr     string
	Password string
	DB       int
	// PoolSize caps this pod's concurrent Redis connections (redis.pool_size
	// config key). 0 keeps the go-redis default (10).
	PoolSize int
}

// New creates a Client and verifies the connection with PING.
func New(ctx context.Context, cfg Config) (*Client, error) {
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.Addr,
		Password: cfg.Password,
		DB:       cfg.DB,
		PoolSize: cfg.PoolSize,
		// Make a caller's ctx deadline bind on the wire. Without this,
		// go-redis only consults ctx BETWEEN operations — the socket deadline
		// is the static ReadTimeout (default 3s), so a hot-path call under a
		// 25ms ctx budget could still stall for the full server-side latency
		// (2026-08-05: DSP/SSP segment lookups hit 214ms under load with the
		// 25ms cap provably not binding — this was the leak).
		ContextTimeoutEnabled: true,
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

// MGet bulk-reads keys in one round trip (cache.BulkGetter). A nil entry
// means the key does not exist. Used by background cache refreshers so the
// per-key read cost is one RTT total, not one RTT each.
func (c *Client) MGet(ctx context.Context, keys ...string) ([]*string, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	vals, err := c.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}
	out := make([]*string, len(keys))
	for i, v := range vals {
		if s, ok := v.(string); ok {
			s := s
			out[i] = &s
		}
	}
	return out, nil
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

func (c *Client) SAdd(ctx context.Context, key string, members ...string) error {
	if len(members) == 0 {
		return nil
	}
	args := make([]any, len(members))
	for i, m := range members {
		args[i] = m
	}
	return c.rdb.SAdd(ctx, key, args...).Err()
}

func (c *Client) SRem(ctx context.Context, key string, members ...string) error {
	if len(members) == 0 {
		return nil
	}
	args := make([]any, len(members))
	for i, m := range members {
		args[i] = m
	}
	return c.rdb.SRem(ctx, key, args...).Err()
}

func (c *Client) SCard(ctx context.Context, key string) (int64, error) {
	return c.rdb.SCard(ctx, key).Result()
}

func (c *Client) SMembers(ctx context.Context, key string) ([]string, error) {
	return c.rdb.SMembers(ctx, key).Result()
}

// ReplaceSet atomically swaps the set at key via a MULTI/EXEC (DEL + SADD +
// EXPIRE) so a concurrent SMEMBERS never sees an empty or half-built set.
func (c *Client) ReplaceSet(ctx context.Context, key string, members []string, ttl time.Duration) error {
	pipe := c.rdb.TxPipeline()
	pipe.Del(ctx, key)
	if len(members) > 0 {
		args := make([]any, len(members))
		for i, m := range members {
			args[i] = m
		}
		pipe.SAdd(ctx, key, args...)
		if ttl > 0 {
			pipe.Expire(ctx, key, ttl)
		}
	}
	_, err := pipe.Exec(ctx)
	return err
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
