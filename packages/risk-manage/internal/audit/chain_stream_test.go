package audit

import (
	"context"
	"testing"
)

// TestChainWriter_HappyPathAndTamper 验证：
//  1. happy path：3 条 record 顺序上链，VerifyStreamChain 通过
//  2. 篡改 body → VerifyStreamChain 在被改的那条返 err
//  3. cross-stream isolation：review 链跟 outcomes 链互不影响
func TestChainWriter_HappyPathAndTamper(t *testing.T) {
	mem := NewMemStreamSink(64)
	cw := NewChainWriter("review", mem)

	for i, op := range []string{"claim", "note", "decide"} {
		if err := cw.Append(context.Background(), map[string]any{
			"type":    "review",
			"op":      op,
			"case_id": "case1",
			"seq":     i,
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	// Recent 是 newest-first；verify 需 oldest-first
	recent := mem.Recent(10)
	if len(recent) != 3 {
		t.Fatalf("expected 3 records, got %d", len(recent))
	}
	records := make([]*StreamRecord, len(recent))
	for i, r := range recent {
		records[len(recent)-1-i] = r
	}
	if idx, err := VerifyStreamChain(records, ""); err != nil {
		t.Fatalf("happy path should verify, fail at %d: %v", idx, err)
	}

	// 篡改第 1 条 body 里的 op 字段（mutable map）→ 后续行 prev_hash 仍正确，
	// 但本行 row_hash 算出来 ≠ 落库 row_hash → 在第 1 条就 fail。
	records[1].Body["op"] = "tampered"
	idx, err := VerifyStreamChain(records, "")
	if err == nil {
		t.Fatal("tampered body should fail verification")
	}
	if idx != 1 {
		t.Fatalf("expected fail at idx=1, got %d (%v)", idx, err)
	}
}

func TestChainWriter_StreamsAreIndependent(t *testing.T) {
	// 两条独立链各写 2 条，互不影响：reviewMem 的 verify 不应被 outcomesMem 干扰
	reviewMem := NewMemStreamSink(16)
	outcomeMem := NewMemStreamSink(16)
	rcw := NewChainWriter("review", reviewMem)
	ocw := NewChainWriter("outcomes", outcomeMem)

	_ = rcw.Append(context.Background(), map[string]any{"op": "claim", "id": "a"})
	_ = ocw.Append(context.Background(), map[string]any{"src": "review_human", "id": "a"})
	_ = rcw.Append(context.Background(), map[string]any{"op": "decide", "id": "a"})
	_ = ocw.Append(context.Background(), map[string]any{"src": "dispute", "id": "a"})

	// review 链单独 verify ok
	revRecent := reviewMem.Recent(10)
	revRecords := make([]*StreamRecord, len(revRecent))
	for i, r := range revRecent {
		revRecords[len(revRecent)-1-i] = r
	}
	if idx, err := VerifyStreamChain(revRecords, ""); err != nil {
		t.Fatalf("review chain should verify, fail at %d: %v", idx, err)
	}

	// outcomes 链单独 verify ok
	oRecent := outcomeMem.Recent(10)
	oRecords := make([]*StreamRecord, len(oRecent))
	for i, r := range oRecent {
		oRecords[len(oRecent)-1-i] = r
	}
	if idx, err := VerifyStreamChain(oRecords, ""); err != nil {
		t.Fatalf("outcomes chain should verify, fail at %d: %v", idx, err)
	}

	// 篡改 review 链不影响 outcomes 链 verify
	revRecords[0].Body["op"] = "evil"
	if _, err := VerifyStreamChain(revRecords, ""); err == nil {
		t.Fatal("tampered review chain should fail")
	}
	if _, err := VerifyStreamChain(oRecords, ""); err != nil {
		t.Fatal("outcomes chain should still verify (independent)")
	}
}

// TestChainWriter_NoopWhenNil cfg 关闭场景：cw==nil 时调用安全（don't panic）。
func TestChainWriter_NoopWhenNil(t *testing.T) {
	var cw *ChainWriter
	if err := cw.Append(context.Background(), map[string]any{"k": "v"}); err != nil {
		t.Fatalf("nil ChainWriter should be no-op, got err=%v", err)
	}
}

// TestRuleAuditChainWrapper Write 走 wrapper → inner 落 + 上链。
func TestRuleAuditChainWrapper(t *testing.T) {
	mem := NewMemStreamSink(8)
	cw := NewChainWriter("rule_audit", mem)
	base := NewMemRuleAuditStore(8)
	wrapped := WrapRuleAuditWithChain(base, cw)

	if err := wrapped.Write(context.Background(), RuleAuditEntry{
		Action: "rule_update", Actor: "alice", RuleID: "r1", Reason: "tweak",
	}); err != nil {
		t.Fatal(err)
	}
	if len(base.Recent(0)) != 1 {
		t.Fatal("inner should have recorded 1 entry")
	}
	recent := mem.Recent(10)
	if len(recent) != 1 {
		t.Fatalf("expected 1 chain record, got %d", len(recent))
	}
	if recent[0].Body["op"] != "rule_update" {
		t.Fatalf("unexpected chain body: %+v", recent[0].Body)
	}
	if idx, err := VerifyStreamChain([]*StreamRecord{recent[0]}, ""); err != nil {
		t.Fatalf("chain verify failed at %d: %v", idx, err)
	}
}
