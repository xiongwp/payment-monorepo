package store

import (
	"context"
	"math"
	"testing"
	"time"
)

// inject 用 reflection 风格直接戳 linkMeta，模拟"X 天前观察到的边"。
// 真实调用 Link() 只能写"now"，没法回测时间衰减；测试白盒触一下没问题。
func (s *MemLinkStore) injectEdge(src, dst string, lastObserved time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 同时写 links 和 linkMeta，保持两个 map 一致
	if _, ok := s.links[src]; !ok {
		s.links[src] = map[string]time.Time{}
	}
	s.links[src][dst] = lastObserved
	if _, ok := s.linkMeta[src]; !ok {
		s.linkMeta[src] = map[string]*edgeMeta{}
	}
	s.linkMeta[src][dst] = &edgeMeta{
		FirstObserved: lastObserved,
		LastObserved:  lastObserved,
		ObserveCount:  1,
	}
}

// TestWeightedFanout_MonotonicDecay 边在 0/15/30/60/90 天前，halflife=30 天，
// 各自单独算 weight：应该单调递减且数值 = e^(-Δd/30)。
//
// 注意：linkTTL 在 mem 实现里默认 1h（hard TTL），injectEdge 写的 30-90 天前
// 的边在 TTL 检查时已经过期，会被立即清掉返回空。本测试**故意验证当前 TTL
// + 衰减共存时的行为**：weight 计算应优先按"还在 TTL 内"分支走。所以这里
// 把 ages 限制在 50min 内（< 1h linkTTL），用 halflife=10min 拉开数值差。
func TestWeightedFanout_MonotonicDecay(t *testing.T) {
	ctx := context.Background()
	halflife := 10 * time.Minute
	now := time.Now()

	// ages 单位：分钟。10/20/30/40/50 分钟前 → weight = e^(-1), e^(-2), ...
	ages := []int{0, 10, 20, 30, 40}
	weights := make([]float64, len(ages))
	for i, m := range ages {
		s := NewMemLinkStore()
		s.injectEdge("device:x", "customer:p", now.Add(-time.Duration(m)*time.Minute))
		w, c := s.WeightedFanout(ctx, "device:x", "customer:", halflife)
		if c != 1 {
			t.Fatalf("age %dmin: expected count=1, got %d weight=%.6f", m, c, w)
		}
		weights[i] = w
		expected := math.Exp(-float64(m) / 10.0)
		if math.Abs(w-expected) > 0.02 {
			t.Errorf("age %dmin: weight %.4f, want ~%.4f", m, w, expected)
		}
	}
	for i := 1; i < len(weights); i++ {
		if weights[i] >= weights[i-1] {
			t.Fatalf("weight not monotonically decreasing: %v", weights)
		}
	}
	// 1×halflife 后应 ≈ 0.368；2× ≈ 0.135
	if math.Abs(weights[1]-math.Exp(-1)) > 0.02 {
		t.Errorf("1×halflife should be ~0.368, got %.4f", weights[1])
	}
}

// TestWeightedFanout_Sum 同一 src 多条边，weight 应该累加。
// 用 halflife=10min，"老"边 = 10 分钟前（1×halflife），保证 < linkTTL=1h。
func TestWeightedFanout_Sum(t *testing.T) {
	s := NewMemLinkStore()
	ctx := context.Background()
	halflife := 10 * time.Minute
	now := time.Now()
	// 5 条"现在"的边 + 5 条"1 halflife 前"的边
	for i := 0; i < 5; i++ {
		s.injectEdge("device:hub", customerKey(i), now)
	}
	for i := 5; i < 10; i++ {
		s.injectEdge("device:hub", customerKey(i), now.Add(-halflife))
	}
	w, c := s.WeightedFanout(ctx, "device:hub", "customer:", halflife)
	if c != 10 {
		t.Fatalf("expected 10 peers, got %d (w=%.4f)", c, w)
	}
	// 期望：5 × 1.0 + 5 × e^-1 ≈ 5 + 5×0.368 = 6.84
	expected := 5.0 + 5*math.Exp(-1)
	if math.Abs(w-expected) > 0.1 {
		t.Errorf("expected weighted sum ~%.2f, got %.4f", expected, w)
	}
}

// TestWeightedFanout_PrefixFilter 验证 peerPrefix 过滤后只计 matching 边
// （但 weight 和 count 不含其他维度）。
func TestWeightedFanout_PrefixFilter(t *testing.T) {
	s := NewMemLinkStore()
	ctx := context.Background()
	halflife := 30 * 24 * time.Hour
	now := time.Now()
	s.injectEdge("device:x", "customer:1", now)
	s.injectEdge("device:x", "ip:1.2.3.4", now)
	s.injectEdge("device:x", "merchant:m1", now)

	w, c := s.WeightedFanout(ctx, "device:x", "customer:", halflife)
	if c != 1 || math.Abs(w-1.0) > 0.01 {
		t.Fatalf("customer prefix: w=%.2f c=%d", w, c)
	}
	w, c = s.WeightedFanout(ctx, "device:x", "", halflife)
	if c != 3 {
		t.Fatalf("no prefix: expected 3, got %d", c)
	}
	_ = w
}

// TestWeightedFanout_GCThreshold 200 天前的边（weight ≈ e^(-200/30) ≈ 1.2e-3 < 1e-3）
// 应该被懒 GC 删掉。这里 linkTTL=1h 会先打回去，所以用 hl=halflife<<linkTTL 让
// 衰减阈值早于 TTL 触发。换思路：手工构造"刚过 7×halflife"的边但 LastObserved 还在 TTL 内，
// 由于 linkTTL 在 Mem 实现里是 1h（生产值小），我们用 hl=10min 让 7×hl=70min < TTL ≈ 1h 时
// 既未过 TTL，又被衰减阈值清掉。
func TestWeightedFanout_GCThreshold(t *testing.T) {
	s := NewMemLinkStore()
	ctx := context.Background()
	// hl = 5min → 7×hl = 35min。linkTTL = 1h，所以 35min 前的边 TTL 还没过期。
	hl := 5 * time.Minute
	now := time.Now()
	s.injectEdge("device:x", "customer:fresh", now)
	s.injectEdge("device:x", "customer:old", now.Add(-40*time.Minute)) // e^(-8) ≈ 3.4e-4 < 1e-3

	w, c := s.WeightedFanout(ctx, "device:x", "customer:", hl)
	if c != 1 {
		t.Fatalf("expected old edge GC'd; got count=%d weight=%.6f", c, w)
	}
	// 验证 old 边真的被从 linkMeta 删掉了
	s.mu.Lock()
	if _, exists := s.linkMeta["device:x"]["customer:old"]; exists {
		s.mu.Unlock()
		t.Fatal("old edge should be lazily GC'd")
	}
	s.mu.Unlock()
}

// TestWeightedFanout_HalflifeZero 半衰期 <=0 时退化为 unweighted （所有 weight=1）。
func TestWeightedFanout_HalflifeZero(t *testing.T) {
	s := NewMemLinkStore()
	ctx := context.Background()
	now := time.Now()
	for i := 0; i < 3; i++ {
		s.injectEdge("device:x", customerKey(i), now.Add(-time.Duration(i+1)*15*time.Minute))
	}
	w, c := s.WeightedFanout(ctx, "device:x", "customer:", 0)
	if c != 3 || math.Abs(w-3.0) > 0.01 {
		t.Fatalf("halflife=0 should give unweighted: w=%.2f c=%d", w, c)
	}
}

// TestWeightedPeersWithin_MultiHop 2 跳 BFS：A→B→C，B 是 customer，C 是 device。
// 全部边在 linkTTL=1h 内。验证 2 跳总权重 > 1 跳。
func TestWeightedPeersWithin_MultiHop(t *testing.T) {
	s := NewMemLinkStore()
	ctx := context.Background()
	hl := 10 * time.Minute
	now := time.Now()
	// device:A -- (now) -- customer:1 -- (10min ago) -- device:B
	s.injectEdge("device:A", "customer:1", now)
	s.injectEdge("customer:1", "device:A", now)
	s.injectEdge("customer:1", "device:B", now.Add(-10*time.Minute))
	s.injectEdge("device:B", "customer:1", now.Add(-10*time.Minute))

	w1, _ := s.WeightedPeersWithin(ctx, "device:A", 1, "", hl, 0.5)
	w2, _ := s.WeightedPeersWithin(ctx, "device:A", 2, "", hl, 0.5)
	if w2 <= w1 {
		t.Fatalf("2-hop weight (%.4f) should exceed 1-hop (%.4f) because B adds signal", w2, w1)
	}
	// w1: 仅 customer:1 with weight = 1.0 × 0.5 = 0.5
	if math.Abs(w1-0.5) > 0.05 {
		t.Errorf("1-hop weight: expected ~0.5, got %.4f", w1)
	}
}

// TestWeightedFanout_OldFanoutBehaviorUnchanged unweighted Peers 应该完全不受
// decay 影响（接口契约：老路径不变）。
func TestWeightedFanout_OldFanoutBehaviorUnchanged(t *testing.T) {
	s := NewMemLinkStore()
	ctx := context.Background()
	now := time.Now()
	// 注入 5 条都"刚刚"的边（保证 linkTTL=1h 内）
	for i := 0; i < 5; i++ {
		s.injectEdge("device:x", customerKey(i), now)
	}
	got := s.Peers(ctx, "device:x", "customer:")
	if len(got) != 5 {
		t.Fatalf("unweighted Peers: expected 5, got %d", len(got))
	}
	// PeersWithin 也应保持不变
	got2 := s.PeersWithin(ctx, "device:x", 1, "customer:")
	if len(got2) != 5 {
		t.Fatalf("unweighted PeersWithin: expected 5, got %d", len(got2))
	}
}

// TestGCDecayed 主动 GC：halflife=5min，注入一批"40 分钟前"的边 → 全清。
func TestGCDecayed(t *testing.T) {
	s := NewMemLinkStore()
	now := time.Now()
	for i := 0; i < 10; i++ {
		s.injectEdge("device:x", customerKey(i), now.Add(-40*time.Minute))
	}
	// fresh 也加一条
	s.injectEdge("device:x", "customer:fresh", now)

	removed := s.GCDecayed(5 * time.Minute)
	if removed != 10 {
		t.Fatalf("expected 10 GC'd, got %d", removed)
	}
	s.mu.Lock()
	remaining := len(s.linkMeta["device:x"])
	s.mu.Unlock()
	if remaining != 1 {
		t.Fatalf("expected 1 fresh edge to remain, got %d", remaining)
	}
}

// TestWeightedTagsWithin_Decay 邻居 tag 应该按 hop + 时间双重衰减。
// 用 hl=10min 让"老边"落在 linkTTL 内。
func TestWeightedTagsWithin_Decay(t *testing.T) {
	s := NewMemLinkStore()
	ctx := context.Background()
	hl := 10 * time.Minute
	now := time.Now()
	// A -- (now) --> B; B 有 fraud tag
	s.injectEdge("n:A", "n:B", now)
	s.injectEdge("n:B", "n:A", now)
	s.mu.Lock()
	s.tags["n:B"] = map[string]time.Time{"fraud": now}
	s.mu.Unlock()

	signals := s.WeightedTagsWithin(ctx, "n:A", 1, hl, 0.5)
	// 1 hop, time_decay ≈ 1, hop_decay = 0.5 → signal ≈ 0.5
	if math.Abs(signals["fraud"]-0.5) > 0.05 {
		t.Errorf("1-hop fresh tag: expected ~0.5, got %.4f", signals["fraud"])
	}

	// 让 A→B 这条边变成 1 halflife 前观察到 (10min)
	s2 := NewMemLinkStore()
	s2.injectEdge("n:A", "n:B", now.Add(-hl))
	s2.injectEdge("n:B", "n:A", now.Add(-hl))
	s2.mu.Lock()
	s2.tags["n:B"] = map[string]time.Time{"fraud": now}
	s2.mu.Unlock()
	signals2 := s2.WeightedTagsWithin(ctx, "n:A", 1, hl, 0.5)
	// 1 hop, time_decay = e^-1 ≈ 0.368, hop_decay = 0.5 → signal ≈ 0.184
	expected := math.Exp(-1) * 0.5
	if math.Abs(signals2["fraud"]-expected) > 0.05 {
		t.Errorf("1-hop aged-edge tag: expected ~%.4f, got %.4f", expected, signals2["fraud"])
	}
}
