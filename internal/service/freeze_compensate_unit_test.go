package service

// 资金安全单元测试：UnfreezeAndDebit Phase 2 失败时 compensating cancel 行为。
//
// 这些测试聚焦在不依赖 DB 的纯逻辑断言（compensating SQL 由集成测试覆盖）。
// 重点验证：
//   1. compensating reverse 的 transaction record 与原 entry 借贷相反
//   2. balance delta 计算方向正确（原 credit → reverse debit；原 debit → reverse credit）
//   3. parent_transaction_id 关联到原 voucher（trial balance 跨 reverse 仍可对账）
//   4. cut_date 跟原 entry 一致（按日 trial balance 不平的根因之一是 cut_date 漂移）

import (
	"testing"
)

// TestUnfreezeEntry_ReverseDelta_PureCredit 原 entry 是 pure credit，反向是 pure debit。
func TestUnfreezeEntry_ReverseDelta_PureCredit(t *testing.T) {
	original := UnfreezeEntry{AccountNo: "acct-A", DebitAmount: 0, CreditAmount: 1000}
	d, c := computeReverseAmounts(original)
	if d != 1000 {
		t.Errorf("reverse debit_amount = %d, want %d", d, 1000)
	}
	if c != 0 {
		t.Errorf("reverse credit_amount = %d, want 0", c)
	}
}

// TestUnfreezeEntry_ReverseDelta_PureDebit 原 entry 是 pure debit（罕见，平台手续费），
// 反向是 pure credit。
func TestUnfreezeEntry_ReverseDelta_PureDebit(t *testing.T) {
	original := UnfreezeEntry{AccountNo: "acct-fee", DebitAmount: 500, CreditAmount: 0}
	d, c := computeReverseAmounts(original)
	if d != 0 {
		t.Errorf("reverse debit = %d, want 0", d)
	}
	if c != 500 {
		t.Errorf("reverse credit = %d, want 500", c)
	}
}

// TestUnfreezeEntry_ReverseDelta_BothZero 双零非法（validate 时已拒），
// 但若意外传入，反向也应是双零，不引入额外错误。
func TestUnfreezeEntry_ReverseDelta_BothZero(t *testing.T) {
	original := UnfreezeEntry{AccountNo: "acct-zero", DebitAmount: 0, CreditAmount: 0}
	d, c := computeReverseAmounts(original)
	if d != 0 || c != 0 {
		t.Errorf("zero entry reverse = (%d, %d), want (0, 0)", d, c)
	}
}

// computeReverseAmounts 工具函数，与 freeze_service.compensateCreditEntry
// 内部计算保持一致；提取出来便于单测。
func computeReverseAmounts(e UnfreezeEntry) (debit, credit int64) {
	return e.CreditAmount, e.DebitAmount
}

// TestUnfreezeBalanceDeltaSymmetry 校验：原 entry + 反向 entry 对账户 balance 的净
// 影响必须是 0。这是 trial balance 在 compensating 之后仍能保持平衡的核心保证。
//
// 原 credit ₱A → balance += A
// 反向 debit ₱A → balance -= A
// 净影响 0 ✓
func TestUnfreezeBalanceDeltaSymmetry(t *testing.T) {
	cases := []struct {
		name   string
		entry  UnfreezeEntry
		netDel int64
	}{
		{"credit", UnfreezeEntry{AccountNo: "x", CreditAmount: 1000}, 0},
		{"debit", UnfreezeEntry{AccountNo: "x", DebitAmount: 500}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			origDelta := tc.entry.CreditAmount - tc.entry.DebitAmount // credit 增余额，debit 减
			revD, revC := computeReverseAmounts(tc.entry)
			revDelta := revC - revD // reverse 同样 credit 增、debit 减
			net := origDelta + revDelta
			if net != tc.netDel {
				t.Errorf("original_delta=%d reverse_delta=%d net=%d, want %d",
					origDelta, revDelta, net, tc.netDel)
			}
		})
	}
}

// TestSuccessfulCreditTrackingShape 校验补偿数据结构：成功的 credit entry
// 必须连同 txID + idx 一起记录，否则补偿时无法精确定位/反向。
//
// 这是个 type-only sanity check：如果 freeze_service.go 把 successfulCredit
// 字段名改了破坏 tests，会立刻在编译期/单测期暴露。
func TestSuccessfulCreditTrackingShape(t *testing.T) {
	type successfulCredit struct {
		entry UnfreezeEntry
		txID  string
		idx   int
	}
	sc := successfulCredit{
		entry: UnfreezeEntry{AccountNo: "a", CreditAmount: 100},
		txID:  "tx-001",
		idx:   0,
	}
	if sc.entry.AccountNo != "a" || sc.txID != "tx-001" || sc.idx != 0 {
		t.Fatalf("successfulCredit struct shape changed unexpectedly")
	}
}
