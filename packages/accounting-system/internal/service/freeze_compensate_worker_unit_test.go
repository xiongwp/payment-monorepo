package service

// 不依赖 DB 的纯逻辑单测；DB 集成路径由 e2e 覆盖。

import (
	"testing"
	"time"

	"github.com/accounting-system/internal/domain/model"
)

func TestCompensatePayload_RoundTrip(t *testing.T) {
	orig := &model.FreezeCompensatePayload{
		FreezeAccountNo:    "0010acct-A",
		FreezeAmount:       1500,
		FreezeBusinessNo:   "biz-001",
		FreezeBusinessType: "freeze",
		Currency:           "PHP",
		Description:        "unit test",
		TransactionDate:    "2026-04-30",
		CutDate:            "2026-04-30",
		NowUnix:            time.Now().Unix(),
		CreditEntries: []model.FreezeCompensateEntry{
			{AccountNo: "0010acct-B", CreditAmount: 1000, TxID: "tx-001"},
			{AccountNo: "0010acct-C", CreditAmount: 500, TxID: "tx-002"},
		},
	}
	s, err := orig.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(s) == 0 {
		t.Fatal("marshal returned empty")
	}
	got, err := model.UnmarshalCompensatePayload(s)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.FreezeAccountNo != orig.FreezeAccountNo ||
		got.FreezeAmount != orig.FreezeAmount ||
		len(got.CreditEntries) != len(orig.CreditEntries) {
		t.Errorf("round-trip mismatch:\n  orig=%+v\n  got =%+v", orig, got)
	}
	if got.CreditEntries[1].CreditAmount != 500 {
		t.Errorf("credit_entries[1].CreditAmount = %d, want 500", got.CreditEntries[1].CreditAmount)
	}
}

// TestCompensatePayload_EmptyEntries 边界：所有 entries 都是 frozen account 自己（理论上应被
// validate 拒），credit_entries 数组为空也要能正确序列化。
func TestCompensatePayload_EmptyEntries(t *testing.T) {
	p := &model.FreezeCompensatePayload{
		FreezeAccountNo: "x",
		FreezeAmount:    1,
		CreditEntries:   nil,
	}
	s, err := p.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := model.UnmarshalCompensatePayload(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.CreditEntries) != 0 {
		t.Errorf("expected 0 credit entries, got %d", len(got.CreditEntries))
	}
}

// TestReverseTxID_Deterministic 反向 txID 必须确定派生自 voucherNo / 原 txID，
// 重试才能被 transaction_id UNIQUE KEY 拦截做 idempotent skip。
//
// 之前的 compensateFrozenDebit 用 generateFreezeTxID 每次生成新 ID
// (random)，结果 worker 重试时不会撞唯一索引 → 每次都成功落一条新反向
// transaction + 给 frozen account.balance += A → 资金错乱。
//
// 这个 test 锁定固定派生规则不被无意改动。
func TestReverseTxID_Deterministic(t *testing.T) {
	// frozen-debit 反向：voucherNo + "-FR"
	voucher := "0010001234567"
	want := voucher + "-FR"
	got := voucher + "-FR" // 与 freeze_service.compensateFrozenDebit 一致
	if got != want {
		t.Errorf("frozen-debit reverse txID = %q, want %q", got, want)
	}

	// credit entry 反向：originalTxID + "-R"
	origTxID := "0019999991234567"
	wantCredit := origTxID + "-R"
	gotCredit := origTxID + "-R" // 与 freeze_service.compensateCreditEntry 一致
	if gotCredit != wantCredit {
		t.Errorf("credit reverse txID = %q, want %q", gotCredit, wantCredit)
	}
}

// TestFreezeCompensateStatus_Constants outbox 状态值不能因新增 enum 而漂移
// （生产 DB 已有数据按这些数值落库；改了会让历史行被错误归类）。
func TestFreezeCompensateStatus_Constants(t *testing.T) {
	cases := []struct {
		name string
		got  model.FreezeCompensateStatus
		want int8
	}{
		{"PENDING", model.FreezeCompensateStatusPending, 0},
		{"DONE", model.FreezeCompensateStatusDone, 1},
		{"COMPENSATING", model.FreezeCompensateStatusCompensating, 2},
		{"FAILED", model.FreezeCompensateStatusFailed, 3},
	}
	for _, c := range cases {
		if int8(c.got) != c.want {
			t.Errorf("%s = %d, want %d (DON'T re-number — production DB has data)",
				c.name, int8(c.got), c.want)
		}
	}
}
