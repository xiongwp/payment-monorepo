package extsignal

import (
	"context"
	"testing"
	"time"
)

func TestScoreCache_GetPutRoundtrip(t *testing.T) {
	c := NewScoreCache(time.Hour)
	e := EntityKey{Type: "user", Key: "42"}
	c.Put(ProviderResult{
		Provider: ProviderSift,
		Entity:   e,
		Score:    0.7,
		Reasons:  []string{"velocity"},
	})
	r, ok := c.Get(ProviderSift, e)
	if !ok {
		t.Fatal("expected hit")
	}
	if r.Score != 0.7 {
		t.Fatalf("score wrong: %v", r.Score)
	}
	if r.FetchedAt.IsZero() {
		t.Fatal("FetchedAt should auto-fill")
	}
}

func TestScoreCache_StaleAfterTTL(t *testing.T) {
	c := NewScoreCache(10 * time.Millisecond)
	e := EntityKey{Type: "ip", Key: "1.2.3.4"}
	c.Put(ProviderResult{Provider: ProviderMaxMind, Entity: e, Score: 0.5})
	time.Sleep(20 * time.Millisecond)
	if _, ok := c.Get(ProviderMaxMind, e); ok {
		t.Fatal("expected stale → miss")
	}
}

func TestScoreCache_PerProviderIsolation(t *testing.T) {
	c := NewScoreCache(time.Hour)
	e := EntityKey{Type: "ip", Key: "1.2.3.4"}
	c.Put(ProviderResult{Provider: ProviderSift, Entity: e, Score: 0.3})
	c.Put(ProviderResult{Provider: ProviderMaxMind, Entity: e, Score: 0.8})

	if r, _ := c.Get(ProviderSift, e); r.Score != 0.3 {
		t.Fatalf("sift score wrong: %v", r.Score)
	}
	if r, _ := c.Get(ProviderMaxMind, e); r.Score != 0.8 {
		t.Fatalf("maxmind score wrong: %v", r.Score)
	}
}

func TestScoreCache_Purge(t *testing.T) {
	c := NewScoreCache(time.Hour)
	e1 := EntityKey{Type: "user", Key: "alice"}
	e2 := EntityKey{Type: "user", Key: "bob"}
	c.Put(ProviderResult{Provider: ProviderSift, Entity: e1})
	c.Put(ProviderResult{Provider: ProviderMaxMind, Entity: e1})
	c.Put(ProviderResult{Provider: ProviderSift, Entity: e2})

	n := c.Purge(e1)
	if n != 2 {
		t.Fatalf("expected 2 purged; got %d", n)
	}
	if _, ok := c.Get(ProviderSift, e1); ok {
		t.Fatal("e1 should be gone")
	}
	if _, ok := c.Get(ProviderSift, e2); !ok {
		t.Fatal("e2 should remain")
	}
}

func TestStubProvider_AlwaysReturnsEmpty(t *testing.T) {
	p := NewStubProvider(ProviderSift)
	r, err := p.Lookup(context.Background(), EntityKey{Type: "user", Key: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Provider != ProviderSift || r.Score != 0 {
		t.Fatalf("expected empty result; got %+v", r)
	}
}

func TestCachedLookup_HitsCache(t *testing.T) {
	c := NewScoreCache(time.Hour)
	e := EntityKey{Type: "user", Key: "a"}
	c.Put(ProviderResult{Provider: ProviderSift, Entity: e, Score: 0.42})

	r := CachedLookup(context.Background(), c, NewStubProvider(ProviderSift), e)
	if r.Score != 0.42 {
		t.Fatalf("expected cached score 0.42; got %v", r.Score)
	}
}

func TestCachedLookup_MissesGoToProvider(t *testing.T) {
	c := NewScoreCache(time.Hour)
	e := EntityKey{Type: "user", Key: "novel"}
	r := CachedLookup(context.Background(), c, NewStubProvider(ProviderSift), e)
	if r.Provider != ProviderSift || r.Entity != e {
		t.Fatalf("expected stub result with provider/entity filled; got %+v", r)
	}
	// 缓存应该 populate
	if _, ok := c.Get(ProviderSift, e); !ok {
		t.Fatal("expected cache populated after miss")
	}
}

func TestNilCache_Safe(t *testing.T) {
	var c *ScoreCache
	c.Put(ProviderResult{})
	if _, ok := c.Get(ProviderSift, EntityKey{}); ok {
		t.Fatal("nil cache get should miss")
	}
	if c.Size() != 0 || c.Purge(EntityKey{}) != 0 {
		t.Fatal("nil cache should be safe noop")
	}
}
