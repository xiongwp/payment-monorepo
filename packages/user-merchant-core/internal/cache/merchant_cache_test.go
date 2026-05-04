package cache

import (
	"testing"
	"time"

	"github.com/xiongwp/user-merchant-core/internal/domain"
)

type fakeHook struct {
	hits   int
	misses int
	size   map[string]int
}

func (h *fakeHook) Lookup(_ string, hit bool) {
	if hit {
		h.hits++
	} else {
		h.misses++
	}
}
func (h *fakeHook) Size(k string, n int) {
	if h.size == nil {
		h.size = map[string]int{}
	}
	h.size[k] = n
}

func mkMerchant(id, live, test string) *domain.Merchant {
	return &domain.Merchant{ID: id, LiveKeyHash: live, TestKeyHash: test}
}

func TestPutGetByID(t *testing.T) {
	h := &fakeHook{}
	c := New(10, time.Minute, h)
	m := mkMerchant("mch_1", "", "")
	c.Put(m)
	got, ok := c.GetByID("mch_1")
	if !ok || got.ID != "mch_1" {
		t.Fatalf("want hit, got %v/%v", got, ok)
	}
	if h.hits != 1 {
		t.Fatal("expected 1 hit counter")
	}
}

func TestGetByIDMiss(t *testing.T) {
	h := &fakeHook{}
	c := New(10, time.Minute, h)
	if _, ok := c.GetByID("nope"); ok {
		t.Fatal("expected miss")
	}
	if h.misses != 1 {
		t.Fatal("expected miss counter")
	}
}

func TestLookupByKeyHash(t *testing.T) {
	c := New(10, time.Minute, nil)
	m := mkMerchant("mch_1", "hashlive", "hashtest")
	c.Put(m)
	for _, h := range []string{"hashlive", "hashtest"} {
		got, ok := c.LookupByKeyHash(h)
		if !ok || got.ID != "mch_1" {
			t.Fatalf("hash %s: want hit, got %v/%v", h, got, ok)
		}
	}
	if _, ok := c.LookupByKeyHash("other"); ok {
		t.Fatal("other should miss")
	}
}

func TestInvalidateRefetchesOnNextLookup(t *testing.T) {
	c := New(10, time.Minute, nil)
	m := mkMerchant("mch_1", "h1", "")
	c.Put(m)
	c.Invalidate("mch_1")
	// byID miss
	if _, ok := c.GetByID("mch_1"); ok {
		t.Fatal("byID should miss after invalidate")
	}
	// hashToID 保留，但 byID 查不到 → LookupByKeyHash 应 miss（正是预期降级）
	if _, ok := c.LookupByKeyHash("h1"); ok {
		t.Fatal("LookupByKeyHash should miss after byID invalidated")
	}
}

func TestPutNilIgnored(t *testing.T) {
	c := New(10, time.Minute, nil)
	c.Put(nil)
	c.Put(&domain.Merchant{ID: ""}) // empty id ignored
	byID, _ := c.Len()
	if byID != 0 {
		t.Fatalf("expected empty cache, got %d", byID)
	}
}

func TestPurge(t *testing.T) {
	c := New(10, time.Minute, nil)
	c.Put(mkMerchant("a", "ha", ""))
	c.Put(mkMerchant("b", "hb", ""))
	c.Purge()
	if byID, byHash := c.Len(); byID != 0 || byHash != 0 {
		t.Fatalf("expected purged, got %d/%d", byID, byHash)
	}
}

func TestTTLExpiry(t *testing.T) {
	c := New(10, 50*time.Millisecond, nil)
	c.Put(mkMerchant("mch_1", "h", ""))
	time.Sleep(70 * time.Millisecond)
	if _, ok := c.GetByID("mch_1"); ok {
		t.Fatal("expected expiry")
	}
}

func TestDefaultSizeAndTTL(t *testing.T) {
	c := New(0, 0, nil)
	c.Put(mkMerchant("x", "h", ""))
	if _, ok := c.GetByID("x"); !ok {
		t.Fatal("defaults should still function")
	}
}
