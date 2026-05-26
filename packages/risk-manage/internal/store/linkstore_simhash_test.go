package store

import (
	"context"
	"testing"
)

func TestMemLinkStore_LinkBySimHash_NoopOnZeroOrEmpty(t *testing.T) {
	s := NewMemLinkStore()
	ctx := context.Background()
	// 0 hash 不入索引（防止未指纹化设备聚成一团）
	s.LinkBySimHash(ctx, 0, "customer:1")
	if got := s.PeersBySimHash(ctx, 0, 0, ""); len(got) != 0 {
		t.Fatalf("0 hash should not match anything, got %v", got)
	}
	// 空 dst no-op
	s.LinkBySimHash(ctx, 0xabcd, "")
	if got := s.PeersBySimHash(ctx, 0xabcd, 0, ""); len(got) != 0 {
		t.Fatalf("empty dst should not be stored, got %v", got)
	}
}

func TestMemLinkStore_PeersBySimHash_ExactAndFuzzy(t *testing.T) {
	s := NewMemLinkStore()
	ctx := context.Background()
	base := uint64(0x0F0F_0F0F_0F0F_0F0F)
	near := base ^ uint64(0x7) // 翻 3 个 bit
	far := base ^ uint64(0xFFFF_FFFF_FFFF_FFFF) // 翻 64 bit

	s.LinkBySimHash(ctx, base, "customer:base")
	s.LinkBySimHash(ctx, near, "customer:near")
	s.LinkBySimHash(ctx, far, "customer:far")

	// threshold=0：精确匹配，只命中 base
	exact := s.PeersBySimHash(ctx, base, 0, "")
	if len(exact) != 1 || exact[0] != "customer:base" {
		t.Fatalf("exact match expected only base, got %v", exact)
	}

	// threshold=8：base + near 命中（距离 0 和 3）；far 不命中（距离 64）
	fuzzy := s.PeersBySimHash(ctx, base, 8, "")
	if len(fuzzy) != 2 {
		t.Fatalf("fuzzy threshold=8 expected 2 peers, got %d (%v)", len(fuzzy), fuzzy)
	}
}

func TestMemLinkStore_PeersBySimHash_PrefixFilter(t *testing.T) {
	s := NewMemLinkStore()
	ctx := context.Background()
	h := uint64(0xDEADBEEF_CAFEBABE)
	s.LinkBySimHash(ctx, h, "customer:1")
	s.LinkBySimHash(ctx, h, "customer:2")
	s.LinkBySimHash(ctx, h, "ip:10.0.0.1")

	if got := s.PeersBySimHash(ctx, h, 0, "customer:"); len(got) != 2 {
		t.Fatalf("customer prefix expected 2, got %d (%v)", len(got), got)
	}
	if got := s.PeersBySimHash(ctx, h, 0, "ip:"); len(got) != 1 || got[0] != "ip:10.0.0.1" {
		t.Fatalf("ip prefix expected 1, got %v", got)
	}
}

func TestMemLinkStore_PeersBySimHash_MultipleBucketsAggregated(t *testing.T) {
	s := NewMemLinkStore()
	ctx := context.Background()
	// 两个近邻 bucket（距离 2）各关联不同 customer；query 应同时收
	a := uint64(0x1111_2222_3333_4444)
	b := a ^ uint64(0x3) // 距离 2
	s.LinkBySimHash(ctx, a, "customer:A")
	s.LinkBySimHash(ctx, b, "customer:B")

	got := s.PeersBySimHash(ctx, a, 4, "customer:")
	if len(got) != 2 {
		t.Fatalf("expected 2 peers from neighboring buckets, got %d (%v)", len(got), got)
	}
}

func TestMemLinkStore_PeersBySimHash_QueryWithZeroHashNoMatch(t *testing.T) {
	s := NewMemLinkStore()
	ctx := context.Background()
	s.LinkBySimHash(ctx, 0xabcd, "customer:1")
	// query simhash=0 → 立即返回 nil（未指纹化的请求不应命中任何设备）
	if got := s.PeersBySimHash(ctx, 0, 64, ""); len(got) != 0 {
		t.Fatalf("query simhash=0 should not match, got %v", got)
	}
}
