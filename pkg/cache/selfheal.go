// Package cache - self-healing L2 wrapper.
//
// Every service used to dial Redis once at boot and silently latch MemoryL2
// forever when that single attempt failed — one 3s DNS blip at pod start and
// freq caps / dedup / budgets ran per-pod in-memory until a human bounced the
// pod (the same boot-latch disease as the registry Ping and SSP mgmt-DB bugs).
// SelfHealingL2 keeps the fail-open behaviour (requests are never blocked on
// Redis) but keeps retrying the dial in the background and swaps to the real
// backend the moment it comes up.
package cache

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// l2holder wraps the interface so atomic.Pointer has a concrete type.
type l2holder struct{ c L2Cache }

// SelfHealingL2 is an L2Cache that serves from an in-memory fallback until a
// background dial loop lands the real backend. Counters accumulated in the
// fallback are lost on swap — the same data loss a pod bounce (the old cure)
// caused, but automatic and much earlier.
type SelfHealingL2 struct {
	current atomic.Pointer[l2holder]
	cancel  context.CancelFunc
}

// NewSelfHealingL2 tries dial once synchronously (so the common case — Redis
// is up — behaves exactly like before), and on failure returns immediately
// with a MemoryL2 while retrying dial every retryEvery in the background.
// name labels log lines (usually the service name).
func NewSelfHealingL2(dial func(ctx context.Context) (L2Cache, error), retryEvery time.Duration, name string, log *slog.Logger) *SelfHealingL2 {
	s := &SelfHealingL2{}

	dialCtx, cancelDial := context.WithTimeout(context.Background(), 3*time.Second)
	client, err := dial(dialCtx)
	cancelDial()
	if err == nil {
		s.current.Store(&l2holder{c: client})
		log.Info("redis connected", "service", name)
		return s
	}

	log.Warn("redis unreachable at boot; using in-memory L2 and retrying in background",
		"service", name, "retry_every", retryEvery, "error", err)
	s.current.Store(&l2holder{c: NewMemoryL2()})

	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	go func() {
		ticker := time.NewTicker(retryEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			dialCtx, cancelDial := context.WithTimeout(ctx, 3*time.Second)
			client, err := dial(dialCtx)
			cancelDial()
			if err != nil {
				continue
			}
			s.current.Store(&l2holder{c: client})
			log.Info("redis recovered; switched from in-memory L2 to the real backend "+
				"(fallback-era counters are discarded)", "service", name)
			return
		}
	}()
	return s
}

func (s *SelfHealingL2) get() L2Cache { return s.current.Load().c }

func (s *SelfHealingL2) Get(ctx context.Context, key string) (string, bool, error) {
	return s.get().Get(ctx, key)
}

func (s *SelfHealingL2) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	return s.get().Set(ctx, key, value, ttl)
}

func (s *SelfHealingL2) Delete(ctx context.Context, key string) error {
	return s.get().Delete(ctx, key)
}

func (s *SelfHealingL2) SetNX(ctx context.Context, key, value string, ttl time.Duration) (bool, error) {
	return s.get().SetNX(ctx, key, value, ttl)
}

func (s *SelfHealingL2) Incr(ctx context.Context, key string) (int64, error) {
	return s.get().Incr(ctx, key)
}

func (s *SelfHealingL2) IncrBy(ctx context.Context, key string, n int64) (int64, error) {
	return s.get().IncrBy(ctx, key, n)
}

func (s *SelfHealingL2) DecrBy(ctx context.Context, key string, n int64) (int64, error) {
	return s.get().DecrBy(ctx, key, n)
}

func (s *SelfHealingL2) Expire(ctx context.Context, key string, ttl time.Duration) error {
	return s.get().Expire(ctx, key, ttl)
}

func (s *SelfHealingL2) SAdd(ctx context.Context, key string, members ...string) error {
	return s.get().SAdd(ctx, key, members...)
}

func (s *SelfHealingL2) SRem(ctx context.Context, key string, members ...string) error {
	return s.get().SRem(ctx, key, members...)
}

func (s *SelfHealingL2) SMembers(ctx context.Context, key string) ([]string, error) {
	return s.get().SMembers(ctx, key)
}

func (s *SelfHealingL2) ReplaceSet(ctx context.Context, key string, members []string, ttl time.Duration) error {
	return s.get().ReplaceSet(ctx, key, members, ttl)
}

func (s *SelfHealingL2) Ping(ctx context.Context) error {
	return s.get().Ping(ctx)
}

func (s *SelfHealingL2) Close() error {
	if s.cancel != nil {
		s.cancel()
	}
	return s.get().Close()
}

// Compile-time proof.
var _ L2Cache = (*SelfHealingL2)(nil)
