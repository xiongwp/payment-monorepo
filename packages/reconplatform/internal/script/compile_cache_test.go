package script

import (
	"sync"
	"testing"
)

func mkCs(id string) *CompiledScript { return &CompiledScript{ID: id, Code: "code-" + id} }

func TestCompileCache_HitByID(t *testing.T) {
	c := NewCompileCache(10)
	c.Put("rule_a", "code_a", mkCs("rule_a"))
	got, ok := c.Get("rule_a", "code_a")
	if !ok || got == nil || got.ID != "rule_a" {
		t.Errorf("hit by id failed: %+v ok=%v", got, ok)
	}
	if c.Stats().Hits != 1 {
		t.Error("hits not counted")
	}
}

func TestCompileCache_HitByHash_DifferentID(t *testing.T) {
	c := NewCompileCache(10)
	code := "def check(ctx): return []"
	c.Put("v1", code, mkCs("v1"))
	// 用不同 ID 但相同 code 取 → hash 命中
	got, ok := c.Get("v2_alias", code)
	if !ok || got == nil {
		t.Errorf("hash hit failed: %+v ok=%v", got, ok)
	}
}

func TestCompileCache_MissOnCodeChange(t *testing.T) {
	c := NewCompileCache(10)
	c.Put("rule_a", "code_v1", mkCs("rule_a"))
	// 同 ID 但 code 变了 → miss + 老 entry 驱逐
	got, ok := c.Get("rule_a", "code_v2_changed")
	if ok || got != nil {
		t.Error("code change should miss")
	}
	if c.Stats().Misses != 1 {
		t.Error("misses not counted")
	}
}

func TestCompileCache_LRUEviction(t *testing.T) {
	c := NewCompileCache(3)
	c.Put("a", "code_a", mkCs("a"))
	c.Put("b", "code_b", mkCs("b"))
	c.Put("c", "code_c", mkCs("c"))
	// Touch a → a 变最新, b 变 LRU
	_, _ = c.Get("a", "code_a")
	c.Put("d", "code_d", mkCs("d"))
	// b 应该被驱逐
	if _, ok := c.Get("b", "code_b"); ok {
		t.Error("b should be evicted")
	}
	if _, ok := c.Get("a", "code_a"); !ok {
		t.Error("a should survive (was touched)")
	}
	if c.Stats().Evicted < 1 {
		t.Errorf("evicted count = %d", c.Stats().Evicted)
	}
}

func TestCompileCache_Invalidate(t *testing.T) {
	c := NewCompileCache(10)
	c.Put("a", "code_a", mkCs("a"))
	c.Invalidate("a")
	if _, ok := c.Get("a", "code_a"); ok {
		t.Error("invalidate should remove")
	}
}

func TestCompileCache_HitRate(t *testing.T) {
	c := NewCompileCache(10)
	c.Put("a", "code", mkCs("a"))
	_, _ = c.Get("a", "code")
	_, _ = c.Get("a", "code")
	_, _ = c.Get("missing", "x")
	rate := c.HitRate()
	if rate < 0.6 || rate > 0.7 { // 2/3 ≈ 0.67
		t.Errorf("hit rate %.2f not ~0.67", rate)
	}
}

func TestCompileCache_ConcurrentSafe(t *testing.T) {
	c := NewCompileCache(50)
	var wg sync.WaitGroup
	for w := 0; w < 20; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			key := "rule_" + string(rune('a'+id%5))
			code := "code_" + key
			for i := 0; i < 200; i++ {
				if i%3 == 0 {
					c.Put(key, code, mkCs(key))
				} else {
					_, _ = c.Get(key, code)
				}
			}
		}(w)
	}
	wg.Wait()
	// 不 panic 即可,size 应 <= maxEntries
	if c.Stats().Size > 50 {
		t.Errorf("size %d > maxEntries 50", c.Stats().Size)
	}
}

func TestHashCode_Deterministic(t *testing.T) {
	a := hashCode("hello")
	b := hashCode("hello")
	c := hashCode("hello!")
	if a != b {
		t.Error("not deterministic")
	}
	if a == c {
		t.Error("different strings should differ")
	}
	if hashCode("") != "" {
		t.Error("empty should be empty")
	}
}
