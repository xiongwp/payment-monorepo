package service

import (
	"strconv"
	"testing"
	"time"
)

func mkEntry(plaintext string) decryptCacheEntry {
	return decryptCacheEntry{
		plaintext: []byte(plaintext),
		keyID:     "k1",
		expires:   time.Now().Add(decryptCacheTTL),
	}
}

func TestDecryptCache_GetAfterSet(t *testing.T) {
	c := newDecryptCache()
	defer c.Stop()
	c.set("a", mkEntry("alpha"))
	got, ok := c.get("a")
	if !ok {
		t.Fatal("expected hit")
	}
	if string(got.plaintext) != "alpha" {
		t.Fatalf("expected alpha, got %s", got.plaintext)
	}
}

// 写满 max → 把 k0 access 一次成 MRU → 再插一条新 entry → 期望 k1 被驱逐
// （它现在是真正的 LRU），k0 + new 都还在。
func TestDecryptCache_LRUOrder(t *testing.T) {
	c := newDecryptCache()
	defer c.Stop()

	for i := 0; i < decryptCacheMaxEntries; i++ {
		c.set("k"+strconv.Itoa(i), mkEntry("v"+strconv.Itoa(i)))
	}

	if _, ok := c.get("k0"); !ok {
		t.Fatal("k0 should still be present")
	}

	c.set("new", mkEntry("nv"))

	if _, ok := c.get("k0"); !ok {
		t.Error("k0 (just accessed) should NOT be evicted")
	}
	if _, ok := c.get("k1"); ok {
		t.Error("k1 (LRU after k0 was promoted) should be evicted")
	}
	if _, ok := c.get("new"); !ok {
		t.Error("new entry should be present")
	}
}

func TestDecryptCache_OverwriteSameKey(t *testing.T) {
	c := newDecryptCache()
	defer c.Stop()
	c.set("a", mkEntry("v1"))
	c.set("a", mkEntry("v2"))
	got, ok := c.get("a")
	if !ok || string(got.plaintext) != "v2" {
		t.Fatalf("expected v2, got %s ok=%v", got.plaintext, ok)
	}
}

func TestDecryptCache_ExpiredOnGet(t *testing.T) {
	c := newDecryptCache()
	defer c.Stop()
	e := mkEntry("expiring")
	e.expires = time.Now().Add(-time.Second) // 已过期
	c.set("a", e)
	if _, ok := c.get("a"); ok {
		t.Fatal("expired entry should not hit")
	}
}

func TestDecryptCache_Clear(t *testing.T) {
	c := newDecryptCache()
	defer c.Stop()
	c.set("a", mkEntry("alpha"))
	c.set("b", mkEntry("beta"))
	c.clear()
	if _, ok := c.get("a"); ok {
		t.Error("a should be cleared")
	}
	if _, ok := c.get("b"); ok {
		t.Error("b should be cleared")
	}
	// 清空后还能继续 set/get
	c.set("c", mkEntry("gamma"))
	if _, ok := c.get("c"); !ok {
		t.Error("c should be set after clear")
	}
}
