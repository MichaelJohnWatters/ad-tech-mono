package cache_test

import (
	"context"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
)

func TestL1_SetAndGet(t *testing.T) {
	clk := clock.NewFake(time.Now())
	l1 := cache.NewL1(clk)

	l1.Set("key1", "value1", 5*time.Minute)

	val, ok := l1.Get("key1")
	if !ok || val != "value1" {
		t.Errorf("Get() = %v, %v, want 'value1', true", val, ok)
	}
}

func TestL1_GetMissing(t *testing.T) {
	clk := clock.NewFake(time.Now())
	l1 := cache.NewL1(clk)

	_, ok := l1.Get("nonexistent")
	if ok {
		t.Error("expected miss for nonexistent key")
	}
}

func TestL1_TTLExpiry(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC))
	l1 := cache.NewL1(clk)

	l1.Set("key1", "value1", 5*time.Minute)

	// Before expiry
	val, ok := l1.Get("key1")
	if !ok || val != "value1" {
		t.Error("key should be present before expiry")
	}

	// Advance past expiry
	clk.Advance(6 * time.Minute)

	_, ok = l1.Get("key1")
	if ok {
		t.Error("key should be expired after TTL")
	}
}

func TestL1_NoExpiry(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC))
	l1 := cache.NewL1(clk)

	l1.Set("key1", "value1", 0) // no TTL

	clk.Advance(24 * time.Hour)

	val, ok := l1.Get("key1")
	if !ok || val != "value1" {
		t.Error("key with no TTL should never expire")
	}
}

func TestL1_Delete(t *testing.T) {
	clk := clock.NewFake(time.Now())
	l1 := cache.NewL1(clk)

	l1.Set("key1", "value1", 0)
	l1.Delete("key1")

	_, ok := l1.Get("key1")
	if ok {
		t.Error("key should be deleted")
	}
}

func TestL1_Clear(t *testing.T) {
	clk := clock.NewFake(time.Now())
	l1 := cache.NewL1(clk)

	l1.Set("a", 1, 0)
	l1.Set("b", 2, 0)
	l1.Set("c", 3, 0)
	l1.Clear()

	if _, ok := l1.Get("a"); ok {
		t.Error("a should be cleared")
	}
	if _, ok := l1.Get("b"); ok {
		t.Error("b should be cleared")
	}
}

func TestL1_ClearPrefix(t *testing.T) {
	clk := clock.NewFake(time.Now())
	l1 := cache.NewL1(clk)

	l1.Set("campaign:123", "data1", 0)
	l1.Set("campaign:456", "data2", 0)
	l1.Set("placement:789", "data3", 0)

	l1.ClearPrefix("campaign:")

	if _, ok := l1.Get("campaign:123"); ok {
		t.Error("campaign:123 should be cleared")
	}
	if _, ok := l1.Get("campaign:456"); ok {
		t.Error("campaign:456 should be cleared")
	}
	if _, ok := l1.Get("placement:789"); !ok {
		t.Error("placement:789 should NOT be cleared")
	}
}

func TestMemoryL2_SetGetDelete(t *testing.T) {
	l2 := cache.NewMemoryL2()
	ctx := context.Background()

	l2.Set(ctx, "key1", "value1", 0)

	val, ok, err := l2.Get(ctx, "key1")
	if err != nil || !ok || val != "value1" {
		t.Errorf("Get() = %q, %v, %v, want 'value1', true, nil", val, ok, err)
	}

	l2.Delete(ctx, "key1")

	_, ok, _ = l2.Get(ctx, "key1")
	if ok {
		t.Error("key should be deleted")
	}
}

func TestMemoryL2_SetNX(t *testing.T) {
	l2 := cache.NewMemoryL2()
	ctx := context.Background()

	// First SetNX: should succeed
	isNew, err := l2.SetNX(ctx, "dedup:msg1", "1", time.Hour)
	if err != nil || !isNew {
		t.Error("first SetNX should return true (new)")
	}

	// Second SetNX: should fail (duplicate)
	isNew, err = l2.SetNX(ctx, "dedup:msg1", "1", time.Hour)
	if err != nil || isNew {
		t.Error("second SetNX should return false (duplicate)")
	}
}

func TestMemoryL2_DecrBy(t *testing.T) {
	l2 := cache.NewMemoryL2()
	ctx := context.Background()

	l2.Set(ctx, "dsp:budget:camp_123", "1000", 0)

	val, err := l2.DecrBy(ctx, "dsp:budget:camp_123", 3)
	if err != nil || val != 997 {
		t.Errorf("DecrBy() = %d, %v, want 997, nil", val, err)
	}
}

func TestMemoryL2_Incr(t *testing.T) {
	l2 := cache.NewMemoryL2()
	ctx := context.Background()

	val, _ := l2.Incr(ctx, "fc:user:camp:d")
	if val != 1 {
		t.Errorf("first Incr = %d, want 1", val)
	}

	val, _ = l2.Incr(ctx, "fc:user:camp:d")
	if val != 2 {
		t.Errorf("second Incr = %d, want 2", val)
	}
}

func TestDedupAdapter(t *testing.T) {
	l2 := cache.NewMemoryL2()
	dedup := cache.NewDedupAdapter(l2)
	ctx := context.Background()

	// Mark new
	isNew, err := dedup.MarkProcessed(ctx, "dedup:test:1", time.Hour)
	if err != nil || !isNew {
		t.Error("first mark should be new")
	}

	// Mark duplicate
	isNew, _ = dedup.MarkProcessed(ctx, "dedup:test:1", time.Hour)
	if isNew {
		t.Error("second mark should be duplicate")
	}

	// Unmark
	dedup.UnmarkProcessed(ctx, "dedup:test:1")

	// Mark again (should be new after unmark)
	isNew, _ = dedup.MarkProcessed(ctx, "dedup:test:1", time.Hour)
	if !isNew {
		t.Error("after unmark, should be new again")
	}
}
