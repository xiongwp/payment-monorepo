package audit

import (
	"context"
	"testing"
	"time"
)

// recordingSink 收集所有写入的 record；测试用。
type recordingSink struct{ got []*DecisionAudit }

func (r *recordingSink) Write(_ context.Context, a *DecisionAudit) {
	cp := *a
	if a.Input.Metadata != nil {
		cp.Input.Metadata = make(map[string]string, len(a.Input.Metadata))
		for k, v := range a.Input.Metadata {
			cp.Input.Metadata[k] = v
		}
	}
	r.got = append(r.got, &cp)
}

func TestChainSink_LinksAndVerifies(t *testing.T) {
	cap := &recordingSink{}
	cs := NewChainSink(cap)
	for i := 0; i < 5; i++ {
		cs.Write(context.Background(), &DecisionAudit{
			DecisionID: "id" + string(rune('a'+i)),
			OccurredAt: time.Date(2026, 4, 28, 10, i, 0, 0, time.UTC),
			Verdict:    "ALLOW",
			RiskScore:  i,
		})
	}
	if len(cap.got) != 5 {
		t.Fatalf("expected 5 captured, got %d", len(cap.got))
	}
	// 第 0 条 prev 应该是 genesis
	if cap.got[0].Input.Metadata["chain_prev_hash"] != genesisHash {
		t.Fatalf("genesis missing: %v", cap.got[0].Input.Metadata)
	}
	// chain 完整性
	if idx, err := VerifyChain(cap.got, ""); err != nil {
		t.Fatalf("chain should verify, but failed at %d: %v", idx, err)
	}
}

func TestChainSink_TamperDetected(t *testing.T) {
	cap := &recordingSink{}
	cs := NewChainSink(cap)
	for i := 0; i < 3; i++ {
		cs.Write(context.Background(), &DecisionAudit{
			DecisionID: "id" + string(rune('a'+i)), Verdict: "ALLOW", RiskScore: i,
		})
	}
	// 篡改第 1 条的 RiskScore
	cap.got[1].RiskScore = 999
	idx, err := VerifyChain(cap.got, "")
	if err == nil {
		t.Fatal("tampered record should fail verification")
	}
	if idx != 1 {
		t.Fatalf("expected fail at idx=1, got %d (%v)", idx, err)
	}
}

func TestChainSink_ReorderDetected(t *testing.T) {
	cap := &recordingSink{}
	cs := NewChainSink(cap)
	for i := 0; i < 4; i++ {
		cs.Write(context.Background(), &DecisionAudit{DecisionID: "id" + string(rune('a'+i))})
	}
	// 交换 1 和 2 的位置
	cap.got[1], cap.got[2] = cap.got[2], cap.got[1]
	if idx, err := VerifyChain(cap.got, ""); err == nil {
		t.Fatalf("reordered chain should fail, but verified ok (idx=%d)", idx)
	}
}

func TestChainSink_DeleteDetected(t *testing.T) {
	cap := &recordingSink{}
	cs := NewChainSink(cap)
	for i := 0; i < 5; i++ {
		cs.Write(context.Background(), &DecisionAudit{DecisionID: "id" + string(rune('a'+i))})
	}
	// 删除第 2 条
	withGap := append([]*DecisionAudit{}, cap.got[:2]...)
	withGap = append(withGap, cap.got[3:]...)
	if idx, err := VerifyChain(withGap, ""); err == nil {
		t.Fatalf("deleted-record gap should fail, but verified ok (idx=%d)", idx)
	}
}

func TestChainSink_ResumeFromLastHash(t *testing.T) {
	cap := &recordingSink{}
	cs := NewChainSink(cap)
	for i := 0; i < 3; i++ {
		cs.Write(context.Background(), &DecisionAudit{DecisionID: "id" + string(rune('a'+i))})
	}
	last := cap.got[2].Input.Metadata["chain_row_hash"]

	// 模拟服务重启：用 last hash 续写
	cap2 := &recordingSink{}
	cs2 := NewChainSinkResume(cap2, last)
	cs2.Write(context.Background(), &DecisionAudit{DecisionID: "id_post"})

	combined := append([]*DecisionAudit{}, cap.got...)
	combined = append(combined, cap2.got...)
	if idx, err := VerifyChain(combined, ""); err != nil {
		t.Fatalf("resumed chain should verify, but failed at %d: %v", idx, err)
	}
}

func TestChainSink_CanonicalIgnoresChainFields(t *testing.T) {
	// 同一条 record 写两次到不同 ChainSink 应该产生稳定的 hash（除 prev 影响外）
	a := &DecisionAudit{DecisionID: "x", Verdict: "DENY", RiskScore: 50,
		Input: AuditInput{Metadata: map[string]string{"k": "v"}}}
	h1 := computeRowHash(genesisHash, a)
	// 把 chain_* 元数据塞进去（应被 canonicalize 过滤掉），hash 不变
	a.Input.Metadata["chain_prev_hash"] = "deadbeef"
	a.Input.Metadata["chain_row_hash"] = "feedface"
	h2 := computeRowHash(genesisHash, a)
	if h1 != h2 {
		t.Fatalf("canonicalize must filter chain_* fields:\n h1=%s\n h2=%s", h1, h2)
	}
}
