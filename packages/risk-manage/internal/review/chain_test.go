package review

import (
	"testing"

	"github.com/xiongwp/risk-manage/internal/audit"
)

// TestChainStore_DecideWritesToChain happy path：
//  1. Push 一条 case
//  2. Decide approve → wrapper 应往 ChainWriter 写一条 record
//  3. 链 verify 通过
func TestChainStore_DecideWritesToChain(t *testing.T) {
	mem := audit.NewMemStreamSink(16)
	cw := audit.NewChainWriter("review", mem)
	base := NewMemStore()
	store := WrapWithChain(base, cw)

	if err := store.Push(Item{ID: "case1", MerchantID: "m", Amount: 100, Currency: "PHP"}); err != nil {
		t.Fatal(err)
	}
	// Claim then Decide：两次 state change，应产生 2 条 chain record。
	if _, err := store.Claim("case1", "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Decide("case1", ActionApprove, "alice", "looks ok"); err != nil {
		t.Fatal(err)
	}

	recent := mem.Recent(10)
	if len(recent) != 2 {
		t.Fatalf("expected 2 chain records (claim+decide), got %d", len(recent))
	}

	// verify chain
	records := make([]*audit.StreamRecord, len(recent))
	for i, r := range recent {
		records[len(recent)-1-i] = r
	}
	if idx, err := audit.VerifyStreamChain(records, ""); err != nil {
		t.Fatalf("chain should verify, fail at %d: %v", idx, err)
	}
}

// TestWrapWithChain_NilCWPassthrough cfg 关闭时 cw=nil 应直接返回原 store
// （不应套 wrapper，零开销）。
func TestWrapWithChain_NilCWPassthrough(t *testing.T) {
	base := NewMemStore()
	got := WrapWithChain(base, nil)
	if _, isWrapper := got.(*chainStore); isWrapper {
		t.Fatalf("nil chain writer should passthrough, got wrapped chainStore")
	}
}
