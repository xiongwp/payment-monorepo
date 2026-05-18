package cachelib

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMemoryCache_BasicGetSetDel(t *testing.T) {
	c := NewMemory(time.Minute)
	ctx := context.Background()

	if _, ok, _ := c.Get(ctx, "missing"); ok {
		t.Fatalf("expected miss on first read")
	}
	if err := c.Set(ctx, "k", []byte("v"), 0); err != nil {
		t.Fatalf("set: %v", err)
	}
	v, ok, err := c.Get(ctx, "k")
	if err != nil || !ok || string(v) != "v" {
		t.Fatalf("expected hit v, got ok=%v v=%q err=%v", ok, v, err)
	}
	if err := c.Del(ctx, "k"); err != nil {
		t.Fatalf("del: %v", err)
	}
	if _, ok, _ := c.Get(ctx, "k"); ok {
		t.Fatalf("expected miss after del")
	}
}

func TestMemoryCache_TTLExpiry(t *testing.T) {
	c := NewMemory(0) // no default TTL
	ctx := context.Background()
	_ = c.Set(ctx, "k", []byte("v"), 20*time.Millisecond)
	if _, ok, _ := c.Get(ctx, "k"); !ok {
		t.Fatalf("expected immediate hit")
	}
	time.Sleep(40 * time.Millisecond)
	if _, ok, _ := c.Get(ctx, "k"); ok {
		t.Fatalf("expected miss after TTL")
	}
}

func TestReadThrough_HitsCacheAfterFirstLoad(t *testing.T) {
	c := NewMemory(time.Minute)
	ctx := context.Background()
	var loaderCalls int32
	loader := func(ctx context.Context) (string, error) {
		atomic.AddInt32(&loaderCalls, 1)
		return "value-from-db", nil
	}
	// 第一次 miss → load
	got, err := ReadThrough[string](ctx, c, "k", loader, ReadThroughOptions{TTL: time.Minute})
	if err != nil || got != "value-from-db" {
		t.Fatalf("first call: %q err=%v", got, err)
	}
	// 第二次 → hit, loader 不再调用
	got2, err := ReadThrough[string](ctx, c, "k", loader, ReadThroughOptions{TTL: time.Minute})
	if err != nil || got2 != "value-from-db" {
		t.Fatalf("second call: %q err=%v", got2, err)
	}
	if loaderCalls != 1 {
		t.Errorf("expected 1 loader call, got %d", loaderCalls)
	}
}

func TestReadThrough_Singleflight_PreventsCacheStampede(t *testing.T) {
	c := NewMemory(time.Minute)
	ctx := context.Background()
	var loaderCalls int32
	loader := func(ctx context.Context) (string, error) {
		atomic.AddInt32(&loaderCalls, 1)
		time.Sleep(50 * time.Millisecond) // 模拟慢 DB
		return "value", nil
	}
	// 50 个并发同时 miss
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = ReadThrough[string](ctx, c, "stampede", loader, ReadThroughOptions{TTL: time.Minute})
		}()
	}
	wg.Wait()
	if loaderCalls != 1 {
		t.Errorf("singleflight failed: expected 1 loader call, got %d", loaderCalls)
	}
}

func TestReadThrough_LoaderErrorNotCached(t *testing.T) {
	c := NewMemory(time.Minute)
	ctx := context.Background()
	loader := func(ctx context.Context) (string, error) {
		return "", errors.New("db down")
	}
	_, err := ReadThrough[string](ctx, c, "err", loader, ReadThroughOptions{TTL: time.Minute})
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if _, ok, _ := c.Get(ctx, "err"); ok {
		t.Errorf("error result should NOT be cached")
	}
}

func TestReadThrough_NegativeCacheNotFound(t *testing.T) {
	c := NewMemory(time.Minute)
	ctx := context.Background()
	var loaderCalls int32
	loader := func(ctx context.Context) (string, error) {
		atomic.AddInt32(&loaderCalls, 1)
		return "", ErrNotFound
	}
	// 第一次 → load, 缓存空值
	_, err := ReadThrough[string](ctx, c, "neg", loader, ReadThroughOptions{TTL: 10 * time.Second, CacheNotFound: true})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	// 第二次 → 命中负缓存, loader 不再调
	_, err = ReadThrough[string](ctx, c, "neg", loader, ReadThroughOptions{TTL: 10 * time.Second, CacheNotFound: true})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected cached ErrNotFound, got %v", err)
	}
	if loaderCalls != 1 {
		t.Errorf("negative cache failed: expected 1 loader call, got %d", loaderCalls)
	}
}

func TestNoopCache(t *testing.T) {
	c := NewNoop()
	ctx := context.Background()
	_ = c.Set(ctx, "k", []byte("v"), time.Minute)
	if _, ok, _ := c.Get(ctx, "k"); ok {
		t.Errorf("Noop should never hit")
	}
}
