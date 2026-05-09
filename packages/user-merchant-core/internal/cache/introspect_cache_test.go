package cache

import (
	"sync/atomic"
	"testing"
	"time"
)

type counterHook struct {
	hits, misses atomic.Int64
}

func (c *counterHook) Lookup(hit bool) {
	if hit {
		c.hits.Add(1)
	} else {
		c.misses.Add(1)
	}
}

func mkVal(uid int64, ttl time.Duration) IntrospectCacheValue {
	return IntrospectCacheValue{
		UserID:        uid,
		EmailVerified: true,
		ExpiresAt:     time.Now().Add(ttl),
		Scopes:        []string{"sso", "wallet"},
		Permissions:   []string{"user:read"},
	}
}

func TestIntrospectCache_HitMiss(t *testing.T) {
	h := &counterHook{}
	c := NewIntrospectCache(0, 0, h) // 默认 100k / 30s

	if _, ok := c.Get("jwt-A"); ok {
		t.Fatal("empty cache should miss")
	}
	if h.misses.Load() != 1 {
		t.Fatalf("expected 1 miss, got %d", h.misses.Load())
	}

	c.Put("jwt-A", mkVal(42, time.Minute))
	got, ok := c.Get("jwt-A")
	if !ok || got.UserID != 42 {
		t.Fatalf("expected hit uid=42, got ok=%v val=%+v", ok, got)
	}
	if h.hits.Load() != 1 {
		t.Fatalf("expected 1 hit, got %d", h.hits.Load())
	}
}

// 不缓存 invalid：Put 拒绝 UserID==0 / 已过期 / 空 jwt。
func TestIntrospectCache_RejectsInvalidPut(t *testing.T) {
	c := NewIntrospectCache(0, 0, nil)

	c.Put("", mkVal(1, time.Minute))
	c.Put("jwt", IntrospectCacheValue{UserID: 0, ExpiresAt: time.Now().Add(time.Minute)})
	c.Put("jwt", IntrospectCacheValue{UserID: 1, ExpiresAt: time.Now().Add(-time.Second)})

	if c.Len() != 0 {
		t.Fatalf("invalid Put should not write, got Len=%d", c.Len())
	}
}

// 已过期的条目即使在缓存里也按 miss 处理，并主动驱逐。
func TestIntrospectCache_ExpiredEntryMissed(t *testing.T) {
	c := NewIntrospectCache(0, 0, nil)

	// 直接构造一个 2s 后过期的条目；等到过期后再 Get
	v := IntrospectCacheValue{UserID: 7, ExpiresAt: time.Now().Add(50 * time.Millisecond)}
	c.Put("short", v)

	if _, ok := c.Get("short"); !ok {
		t.Fatal("should hit immediately after Put")
	}

	time.Sleep(80 * time.Millisecond)
	if _, ok := c.Get("short"); ok {
		t.Fatal("expired entry should be miss")
	}
	if c.Len() != 0 {
		t.Fatalf("expired entry should be evicted on access, Len=%d", c.Len())
	}
}

// Logout 单 token 失效。
func TestIntrospectCache_Invalidate(t *testing.T) {
	c := NewIntrospectCache(0, 0, nil)
	c.Put("jwt-A", mkVal(1, time.Minute))
	c.Put("jwt-B", mkVal(1, time.Minute))

	c.Invalidate("jwt-A")
	if _, ok := c.Get("jwt-A"); ok {
		t.Fatal("Invalidate(A) should remove A")
	}
	if _, ok := c.Get("jwt-B"); !ok {
		t.Fatal("Invalidate(A) should not affect B")
	}
}

// LogoutAll：同一 user 多设备 token 全部清理。
func TestIntrospectCache_InvalidateUser(t *testing.T) {
	c := NewIntrospectCache(0, 0, nil)
	c.Put("phone", mkVal(99, time.Minute))
	c.Put("laptop", mkVal(99, time.Minute))
	c.Put("other-user", mkVal(100, time.Minute))

	c.InvalidateUser(99)
	if _, ok := c.Get("phone"); ok {
		t.Fatal("InvalidateUser should clear phone")
	}
	if _, ok := c.Get("laptop"); ok {
		t.Fatal("InvalidateUser should clear laptop")
	}
	if _, ok := c.Get("other-user"); !ok {
		t.Fatal("InvalidateUser should not affect other users")
	}
}

// 返回值不能被外部修改污染缓存（防 caller 改 Permissions 影响下次 Get）。
func TestIntrospectCache_ReturnsCopy(t *testing.T) {
	c := NewIntrospectCache(0, 0, nil)
	c.Put("jwt", mkVal(1, time.Minute))

	got, ok := c.Get("jwt")
	if !ok {
		t.Fatal("should hit")
	}
	got.Permissions[0] = "MUTATED"

	got2, _ := c.Get("jwt")
	if got2.Permissions[0] == "MUTATED" {
		t.Fatal("cache returned shared slice; mutation leaked")
	}
}
