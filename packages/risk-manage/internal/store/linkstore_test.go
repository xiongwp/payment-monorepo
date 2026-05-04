package store

import (
	"context"
	"testing"
	"time"
)

func TestMemLinkStore_BidirectionalLink(t *testing.T) {
	s := NewMemLinkStore()
	ctx := context.Background()
	s.Link(ctx, "device:abc", "customer:42")
	if got := s.Peers(ctx, "device:abc", "customer:"); len(got) != 1 || got[0] != "customer:42" {
		t.Fatalf("device→customer: %v", got)
	}
	if got := s.Peers(ctx, "customer:42", "device:"); len(got) != 1 || got[0] != "device:abc" {
		t.Fatalf("customer→device: %v", got)
	}
}

func TestMemLinkStore_FanoutDevice(t *testing.T) {
	s := NewMemLinkStore()
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		s.Link(ctx, "device:phone", customerKey(i))
	}
	if got := s.Peers(ctx, "device:phone", "customer:"); len(got) != 5 {
		t.Fatalf("expected 5 customers linked, got %d", len(got))
	}
}

func TestMemLinkStore_PrefixFilter(t *testing.T) {
	s := NewMemLinkStore()
	ctx := context.Background()
	s.Link(ctx, "device:x", "customer:1")
	s.Link(ctx, "device:x", "ip:1.2.3.4")

	// 不带前缀 → 全部
	if got := s.Peers(ctx, "device:x", ""); len(got) != 2 {
		t.Fatalf("no prefix: expected 2, got %d", len(got))
	}
	// 仅 customer
	if got := s.Peers(ctx, "device:x", "customer:"); len(got) != 1 || got[0] != "customer:1" {
		t.Fatalf("customer prefix: %v", got)
	}
	// 仅 ip
	if got := s.Peers(ctx, "device:x", "ip:"); len(got) != 1 || got[0] != "ip:1.2.3.4" {
		t.Fatalf("ip prefix: %v", got)
	}
}

func TestMemLinkStore_DedupSamePeer(t *testing.T) {
	s := NewMemLinkStore()
	ctx := context.Background()
	for i := 0; i < 100; i++ {
		s.Link(ctx, "device:x", "customer:1") // 重复 link 同一 peer
	}
	if got := s.Peers(ctx, "device:x", "customer:"); len(got) != 1 {
		t.Fatalf("expected 1 unique peer (dedup), got %d", len(got))
	}
}

func TestMemLinkStore_EmptyArgsNoop(t *testing.T) {
	s := NewMemLinkStore()
	ctx := context.Background()
	s.Link(ctx, "", "customer:1")
	s.Link(ctx, "device:x", "")
	s.Link(ctx, "same", "same")
	if got := s.Peers(ctx, "customer:1", ""); len(got) != 0 {
		t.Fatal("expected no peers from empty arg link")
	}
}

func TestMemLinkStore_PeersExpired(t *testing.T) {
	s := NewMemLinkStore()
	ctx := context.Background()
	// 直接构造一个已过期的 entry：手工写 map（绕过 Link）
	s.mu.Lock()
	s.links["device:x"] = map[string]time.Time{
		"customer:old": time.Now().Add(-2 * time.Hour),
		"customer:new": time.Now(),
	}
	s.mu.Unlock()
	got := s.Peers(ctx, "device:x", "customer:")
	if len(got) != 1 || got[0] != "customer:new" {
		t.Fatalf("expired entry not pruned: %v", got)
	}
	// 验证过期 entry 真被从 bucket 删了
	s.mu.Lock()
	if _, exists := s.links["device:x"]["customer:old"]; exists {
		t.Fatal("expired peer should have been deleted on Peers")
	}
	s.mu.Unlock()
}

func TestMemLinkStore_PeersWithin_2hop(t *testing.T) {
	s := NewMemLinkStore()
	ctx := context.Background()
	// 构造一个 2 跳环：device:A — customer:1 — device:B
	s.Link(ctx, "device:A", "customer:1")
	s.Link(ctx, "customer:1", "device:B")

	// device:A 1 跳 → 只看到 customer:1
	one := s.PeersWithin(ctx, "device:A", 1, "")
	if len(one) != 1 || one[0] != "customer:1" {
		t.Fatalf("1-hop: %v", one)
	}
	// device:A 2 跳 → 看到 customer:1 + device:B
	two := s.PeersWithin(ctx, "device:A", 2, "")
	if len(two) != 2 {
		t.Fatalf("2-hop expected 2, got %v", two)
	}
	// 2 跳 + prefix 只要 device → 只 device:B（device:A 是起点不算）
	twoDev := s.PeersWithin(ctx, "device:A", 2, "device:")
	if len(twoDev) != 1 || twoDev[0] != "device:B" {
		t.Fatalf("2-hop device-only: %v", twoDev)
	}
}

func TestMemLinkStore_PeersWithin_NoDuplicates(t *testing.T) {
	s := NewMemLinkStore()
	ctx := context.Background()
	// 钻石型：A-B, A-C, B-D, C-D；2 跳 D 应只出现一次
	s.Link(ctx, "n:A", "n:B")
	s.Link(ctx, "n:A", "n:C")
	s.Link(ctx, "n:B", "n:D")
	s.Link(ctx, "n:C", "n:D")
	got := s.PeersWithin(ctx, "n:A", 2, "")
	count := 0
	for _, x := range got {
		if x == "n:D" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("D should appear once, got %d in %v", count, got)
	}
}

func TestMemLinkStore_TagsWithin_PropagatesFromNeighbor(t *testing.T) {
	s := NewMemLinkStore()
	ctx := context.Background()
	s.Link(ctx, "customer:victim", "device:shared")
	s.Link(ctx, "device:shared", "customer:fraudster")
	s.Tag(ctx, "customer:fraudster", "fraud")

	// victim 1 跳 → 只到 device:shared，没 fraud tag
	tags1 := s.TagsWithin(ctx, "customer:victim", 1)
	if tags1["fraud"] != 0 {
		t.Fatalf("1-hop should not see fraudster tag yet: %v", tags1)
	}
	// 2 跳 → 看到 fraudster 的 fraud tag
	tags2 := s.TagsWithin(ctx, "customer:victim", 2)
	if tags2["fraud"] != 1 {
		t.Fatalf("2-hop should propagate fraud tag once, got %v", tags2)
	}
}

func TestMemLinkStore_TagsWithin_TTLExpiry(t *testing.T) {
	s := NewMemLinkStore()
	ctx := context.Background()
	s.Link(ctx, "n:A", "n:B")
	s.tags["n:B"] = map[string]time.Time{"fraud": time.Now().Add(-2 * time.Hour)}

	tags := s.TagsWithin(ctx, "n:A", 1)
	if tags["fraud"] != 0 {
		t.Fatalf("expired tag should not propagate: %v", tags)
	}
	// 验证过期 tag 真被清掉
	s.mu.Lock()
	if _, exists := s.tags["n:B"]["fraud"]; exists {
		t.Fatal("expired tag should be deleted")
	}
	s.mu.Unlock()
}
