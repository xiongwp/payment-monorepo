package cache

import (
	"testing"
	"time"
)

type fakeSecretHook struct{ hits, misses int }

func (h *fakeSecretHook) Lookup(hit bool) {
	if hit {
		h.hits++
	} else {
		h.misses++
	}
}

func TestSecretCacheRoundTrip(t *testing.T) {
	h := &fakeSecretHook{}
	c := NewSecretCache(10, time.Minute, h)
	src := map[string]string{"k1": "v1", "k2": "v2"}
	c.Put("mch_1", "gcash", src)
	got, ok := c.Get("mch_1", "gcash")
	if !ok {
		t.Fatal("expected hit")
	}
	if got["k1"] != "v1" || got["k2"] != "v2" {
		t.Fatalf("content mismatch: %v", got)
	}
	// 结果是副本：改返回的 map 不应污染缓存
	got["k1"] = "tamper"
	second, _ := c.Get("mch_1", "gcash")
	if second["k1"] != "v1" {
		t.Fatalf("cache mutated via returned map: %v", second)
	}
}

func TestSecretCacheMiss(t *testing.T) {
	h := &fakeSecretHook{}
	c := NewSecretCache(10, time.Minute, h)
	if _, ok := c.Get("x", "y"); ok {
		t.Fatal("expected miss")
	}
	if h.misses != 1 {
		t.Fatal("miss counter not recorded")
	}
}

func TestSecretCacheInvalidate(t *testing.T) {
	c := NewSecretCache(10, time.Minute, nil)
	c.Put("mch_1", "gcash", map[string]string{"k": "v"})
	c.Put("mch_1", "maya", map[string]string{"k": "v"})
	c.Invalidate("mch_1", "gcash")
	if _, ok := c.Get("mch_1", "gcash"); ok {
		t.Fatal("gcash should be gone")
	}
	if _, ok := c.Get("mch_1", "maya"); !ok {
		t.Fatal("maya should remain")
	}
}

func TestSecretCacheInvalidateMerchant(t *testing.T) {
	c := NewSecretCache(10, time.Minute, nil)
	c.Put("mch_1", "gcash", map[string]string{"k": "v"})
	c.Put("mch_1", "maya", map[string]string{"k": "v"})
	c.Put("mch_2", "gcash", map[string]string{"k": "v"})
	c.InvalidateMerchant("mch_1")
	if _, ok := c.Get("mch_1", "gcash"); ok {
		t.Fatal("mch_1/gcash should be gone")
	}
	if _, ok := c.Get("mch_1", "maya"); ok {
		t.Fatal("mch_1/maya should be gone")
	}
	if _, ok := c.Get("mch_2", "gcash"); !ok {
		t.Fatal("mch_2/gcash should remain")
	}
}
