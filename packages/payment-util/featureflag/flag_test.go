package featureflag

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func newTestFFReal(flags map[string]Flag) *FeatureFlag {
	src := NewMemorySource(flags)
	return New(Config{Source: src, PollInterval: 60 * time.Second})
}

func TestBoolDefault(t *testing.T) {
	ff := newTestFFReal(map[string]Flag{
		"foo": {Name: "foo", Default: true},
		"bar": {Name: "bar", Default: false},
	})
	if !ff.Bool("foo") {
		t.Error("foo should be true")
	}
	if ff.Bool("bar") {
		t.Error("bar should be false")
	}
	if ff.Bool("undefined") {
		t.Error("undefined flag must be false")
	}
}

func TestRolloutPercent(t *testing.T) {
	ff := newTestFFReal(map[string]Flag{
		"gradual": {Name: "gradual", RolloutPercent: 50},
	})
	// 跑 1000 不同 key, 应该 ~50% true
	hits := 0
	for i := 0; i < 1000; i++ {
		if ff.BoolFor(context.Background(), "gradual", fmt.Sprintf("k%d", i)) {
			hits++
		}
	}
	if hits < 400 || hits > 600 {
		t.Errorf("expected ~500 hits, got %d", hits)
	}
}

func TestIncludeExclude(t *testing.T) {
	ff := newTestFFReal(map[string]Flag{
		"f": {Name: "f", RolloutPercent: 0, IncludeKeys: []string{"vip1"}, ExcludeKeys: []string{"baduser"}},
	})
	if !ff.BoolFor(context.Background(), "f", "vip1") {
		t.Error("vip1 should be on (include list)")
	}
	if ff.BoolFor(context.Background(), "f", "baduser") {
		t.Error("baduser should be off (exclude list)")
	}
	if ff.BoolFor(context.Background(), "f", "random") {
		t.Error("random should be off (rollout=0)")
	}
}

func TestRolloutDeterministic(t *testing.T) {
	// 同 key 必须一直得到同结果 (不能这次 true 下次 false)
	ff := newTestFFReal(map[string]Flag{
		"x": {Name: "x", RolloutPercent: 50},
	})
	first := ff.BoolFor(context.Background(), "x", "merchant_abc")
	for i := 0; i < 50; i++ {
		if ff.BoolFor(context.Background(), "x", "merchant_abc") != first {
			t.Fatal("rollout not deterministic for same key")
		}
	}
}

func TestInt(t *testing.T) {
	ff := newTestFFReal(map[string]Flag{
		"size":  {Name: "size", Default: float64(42)}, // JSON 反序列化默认是 float64
		"limit": {Name: "limit", Default: 100},
	})
	if got := ff.Int("size", 1); got != 42 {
		t.Errorf("size=%d want 42", got)
	}
	if got := ff.Int("limit", 1); got != 100 {
		t.Errorf("limit=%d want 100", got)
	}
	if got := ff.Int("missing", 7); got != 7 {
		t.Errorf("missing=%d want default 7", got)
	}
}

func TestStringFlag(t *testing.T) {
	ff := newTestFFReal(map[string]Flag{
		"region": {Name: "region", Default: "us-east-1"},
	})
	if ff.String("region", "default") != "us-east-1" {
		t.Error("region wrong")
	}
	if ff.String("missing", "fallback") != "fallback" {
		t.Error("missing should fall to default")
	}
}

func TestJSON(t *testing.T) {
	ff := newTestFFReal(map[string]Flag{
		"limits": {Name: "limits", Default: map[string]any{
			"max_rps":   float64(100),
			"max_conns": float64(50),
		}},
	})
	type L struct {
		MaxRPS   int `json:"max_rps"`
		MaxConns int `json:"max_conns"`
	}
	var got L
	if err := ff.JSON("limits", &got); err != nil {
		t.Fatal(err)
	}
	if got.MaxRPS != 100 || got.MaxConns != 50 {
		t.Errorf("JSON decode mismatch: %+v", got)
	}
}

func TestHits(t *testing.T) {
	ff := newTestFFReal(map[string]Flag{"x": {Name: "x"}})
	for i := 0; i < 5; i++ {
		_ = ff.Bool("x")
	}
	h := ff.Hits()
	if h["x"] != 5 {
		t.Errorf("expected 5 hits, got %d", h["x"])
	}
}

func TestBucketSalt(t *testing.T) {
	// 同 key 在不同 flag (salt) 下应分到不同桶 — 避免相关 flag 群体一致灰度
	b1 := bucket("merchant_abc", "flag_a")
	b2 := bucket("merchant_abc", "flag_b")
	if b1 == b2 {
		t.Log("note: same bucket for different salt is allowed but rare")
	}
	// 但同 key 同 flag 必须稳定
	if bucket("merchant_abc", "flag_a") != b1 {
		t.Fatal("bucket not deterministic")
	}
}
