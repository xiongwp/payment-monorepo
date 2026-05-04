package featurecache

import (
	"context"
	"testing"
	"time"
)

func TestMemCache_GetSetDelete(t *testing.T) {
	c := NewMemCache(time.Minute)
	ctx := context.Background()
	if _, ok := c.Get(ctx, "k"); ok {
		t.Fatal("expected miss")
	}
	c.Set(ctx, "k", []byte("hello"), time.Minute)
	got, ok := c.Get(ctx, "k")
	if !ok || string(got) != "hello" {
		t.Fatalf("expected hit hello, got ok=%v val=%q", ok, got)
	}
	c.Delete(ctx, "k")
	if _, ok := c.Get(ctx, "k"); ok {
		t.Fatal("expected miss after delete")
	}
}

func TestMemCache_TTLExpires(t *testing.T) {
	c := NewMemCache(time.Minute)
	ctx := context.Background()
	c.Set(ctx, "k", []byte("v"), 1*time.Millisecond)
	time.Sleep(10 * time.Millisecond)
	if _, ok := c.Get(ctx, "k"); ok {
		t.Fatal("expected miss after TTL")
	}
}

func TestMemCache_StoredCopyIsolated(t *testing.T) {
	// 调用者 mutate 传入的 slice 不应该改 cache 里的值。
	c := NewMemCache(time.Minute)
	ctx := context.Background()
	v := []byte("orig")
	c.Set(ctx, "k", v, time.Minute)
	v[0] = 'X'
	got, _ := c.Get(ctx, "k")
	if string(got) != "orig" {
		t.Fatalf("cache stored aliased slice; got %q", got)
	}
}

func TestNoopCache_AlwaysMiss(t *testing.T) {
	var c Cache = NoopCache{}
	ctx := context.Background()
	c.Set(ctx, "k", []byte("v"), time.Minute)
	if _, ok := c.Get(ctx, "k"); ok {
		t.Fatal("noop should always miss")
	}
}
