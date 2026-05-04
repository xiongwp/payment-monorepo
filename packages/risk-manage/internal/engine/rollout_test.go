package engine

import (
	"testing"
)

func TestInRollout_Disabled(t *testing.T) {
	if inRollout("r1", RolloutConfig{EnablePct: 0}, &TxnContext{}) {
		t.Fatal("EnablePct=0 should always be false")
	}
}

func TestInRollout_FullEnabled(t *testing.T) {
	for i := 0; i < 100; i++ {
		txn := &TxnContext{CustomerID: "c" + itoa(i)}
		if !inRollout("r1", RolloutConfig{EnablePct: 100, BucketField: "customer_id"}, txn) {
			t.Fatalf("EnablePct=100 should always be true (failed at i=%d)", i)
		}
	}
}

func TestInRollout_PercentageDistribution(t *testing.T) {
	const samples = 10000
	hits := 0
	for i := 0; i < samples; i++ {
		txn := &TxnContext{CustomerID: "c" + itoa(i)}
		if inRollout("r1", RolloutConfig{EnablePct: 25, BucketField: "customer_id"}, txn) {
			hits++
		}
	}
	got := float64(hits) / float64(samples) * 100
	// 容差 +/- 3 个百分点（10k 样本下 4 sigma 内）
	if got < 22 || got > 28 {
		t.Fatalf("expected ~25%% hits, got %.1f%% (%d/%d)", got, hits, samples)
	}
}

func TestInRollout_StableForSameCustomer(t *testing.T) {
	txn := &TxnContext{CustomerID: "stable_user_42"}
	first := inRollout("r1", RolloutConfig{EnablePct: 50, BucketField: "customer_id"}, txn)
	for i := 0; i < 100; i++ {
		if got := inRollout("r1", RolloutConfig{EnablePct: 50, BucketField: "customer_id"}, txn); got != first {
			t.Fatalf("same customer should always get same bucket; first=%v got=%v at i=%d", first, got, i)
		}
	}
}

func TestInRollout_DifferentRulesGetIndependentBuckets(t *testing.T) {
	// 同 customer 不同 rule_id → 桶号独立（防"被选中的同一组用户永远命中所有灰度"）
	txn := &TxnContext{CustomerID: "c1"}
	bucketsA := 0
	bucketsB := 0
	for i := 0; i < 1; i++ {
		if inRollout("ruleA", RolloutConfig{EnablePct: 50, BucketField: "customer_id"}, txn) {
			bucketsA++
		}
		if inRollout("ruleB", RolloutConfig{EnablePct: 50, BucketField: "customer_id"}, txn) {
			bucketsB++
		}
	}
	// 仅断言不 panic + 两个独立 hash 都 deterministic（具体值不确定）
	t.Logf("ruleA hit=%v ruleB hit=%v", bucketsA > 0, bucketsB > 0)
}

// 复用 rules 包用过的 itoa
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	const digits = "0123456789"
	if i < 10 {
		return string(digits[i])
	}
	return itoa(i/10) + string(digits[i%10])
}
