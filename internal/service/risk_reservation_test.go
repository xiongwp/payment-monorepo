package service

import (
	"context"
	"encoding/json"
	"testing"

	"go.uber.org/zap"

	"github.com/xiongwp/risk-manage/internal/engine"
	"github.com/xiongwp/risk-manage/internal/rules"
	"github.com/xiongwp/risk-manage/internal/store"
)

// 资损修复回归：service.Screen 必须把 ReservationTracker 注入 ctx，
// 让 amount_limit 走原子预扣路径，并在 Deny 时 CancelAll 把预扣回滚。
//
// 三个断言：
//  1. Allow 路径下 counter 被预扣（atomic 路径生效，证明 tracker 真在 ctx 里）
//  2. Deny 路径下 counter 不被预扣（命中限额 → IncrIfBelow 返 false → 不 Add）
//  3. 跑两次都 Allow 然后第三次会撞 daily limit；如果中间 Deny 路径漏 Cancel，
//     之前的预扣会把限额吃光让正常请求也撞上 → 这里反向覆盖

func newSvcWithAmountLimitRule(t *testing.T, counter store.Counter, maxDaily int64) *RiskService {
	t.Helper()
	eng := engine.New(zap.NewNop())
	eng.RegisterFactory("amount_limit", rules.AmountLimitFactory(counter))
	cfgJSON, _ := json.Marshal(map[string]any{
		"max_daily": maxDaily,
		"scope_by":  "customer",
	})
	if err := eng.LoadRules([]engine.RuleDef{{
		ID: "al1", Name: "daily limit", Type: "amount_limit",
		Decision: "deny", Enabled: true, Mode: "enforce", Weight: 100,
		ConfigJSON: cfgJSON,
	}}); err != nil {
		t.Fatalf("LoadRules: %v", err)
	}
	return New(eng, counter, zap.NewNop())
}

func TestScreen_WiresReservationTracker_AllowKeepsPredebit(t *testing.T) {
	counter := store.NewMemCounter()
	svc := newSvcWithAmountLimitRule(t, counter, 1_000_000)

	res := svc.Screen(context.Background(), &engine.TxnContext{
		PaymentIntentID: "pi_1", CustomerID: "c1", Amount: 300_000, MerchantID: "m1",
	})
	if res.Decision != engine.Allow {
		t.Fatalf("want Allow got %s", res.Decision)
	}
	// 预扣应该已落到 counter（证明 tracker 在 ctx 里 → atomic 路径生效）。
	// 注意：MemCounter 没有 GetDaily 直接揭露 atomic 路径写的桶，而是按
	// 同 key 累加。所以 Get 应能读到本笔金额。
	if got := counter.GetDaily(context.Background(), "customer:c1"); got != 300_000 {
		t.Fatalf("Allow path should keep predebit: GetDaily=%d want 300000", got)
	}
}

func TestScreen_WiresReservationTracker_DenyCancels(t *testing.T) {
	counter := store.NewMemCounter()
	// 限额 500k；先把 counter 预填到 400k（模拟历史累计）
	counter.Incr(context.Background(), "customer:c1", 400_000)
	svc := newSvcWithAmountLimitRule(t, counter, 500_000)

	// 本笔 200k → 400k+200k=600k > 500k → IncrIfBelowDaily 返 false → Deny
	res := svc.Screen(context.Background(), &engine.TxnContext{
		PaymentIntentID: "pi_2", CustomerID: "c1", Amount: 200_000, MerchantID: "m1",
	})
	if res.Decision != engine.Deny {
		t.Fatalf("want Deny got %s", res.Decision)
	}
	// IncrIfBelowDaily 失败时不 Add → tracker 没东西可 Cancel；counter 留在 400k。
	if got := counter.GetDaily(context.Background(), "customer:c1"); got != 400_000 {
		t.Fatalf("Deny on IncrIfBelow=false: counter unchanged at 400k, got %d", got)
	}
}

// 关键回归：用 amount_limit 同时作为 "本笔 + 历史 < limit" 的预扣 deny 与
// "deny 后 Cancel 必须把已 Add 的项回滚" 双重场景。
//
// 构造：limit=600k；先 Allow 一笔 200k（Add reservation 200k 到 customer:c1）；
// 然后用同 customer 走第二笔，触发**另一个规则**判 Deny（不撞 amount_limit）—
// 此时 tracker 里有 amount_limit 这一笔的 Add，必须被 CancelAll 回滚到 200k。
//
// 这个场景验证：Cancel 在 Deny 时确实把已经 Add 的回滚，不光在 IncrIfBelow
// 失败时不 Add 那条简单分支。
func TestScreen_DenyByOtherRule_CancelsAmountLimitReservation(t *testing.T) {
	counter := store.NewMemCounter()
	eng := engine.New(zap.NewNop())
	eng.RegisterFactory("amount_limit", rules.AmountLimitFactory(counter))
	eng.RegisterFactory("country_block", rules.CountryBlockFactory())

	cfgAL, _ := json.Marshal(map[string]any{"max_daily": 600_000, "scope_by": "customer"})
	cfgCB, _ := json.Marshal(map[string]any{"mode": "deny", "countries": []string{"NK"}})
	if err := eng.LoadRules([]engine.RuleDef{
		{ID: "al", Name: "al", Type: "amount_limit", Decision: "deny", Enabled: true, Mode: "enforce", Weight: 100, ConfigJSON: cfgAL},
		{ID: "cb", Name: "cb", Type: "country_block", Decision: "deny", Enabled: true, Mode: "enforce", Weight: 100, ConfigJSON: cfgCB},
	}); err != nil {
		t.Fatalf("LoadRules: %v", err)
	}
	svc := New(eng, counter, zap.NewNop())

	// 第一笔：Allow，预扣 200k → counter=200k
	r1 := svc.Screen(context.Background(), &engine.TxnContext{
		PaymentIntentID: "p1", CustomerID: "c1", Amount: 200_000, Country: "JP",
	})
	if r1.Decision != engine.Allow {
		t.Fatalf("p1 want Allow got %s", r1.Decision)
	}
	if got := counter.GetDaily(context.Background(), "customer:c1"); got != 200_000 {
		t.Fatalf("after p1: GetDaily=%d want 200000", got)
	}

	// 第二笔：amount_limit 通过（200k+150k=350k<600k 会 Add 150k 到 reservation），
	// 但 country_block 命中 Deny → Screen 终判 Deny → CancelAll 必须把 150k 回滚。
	r2 := svc.Screen(context.Background(), &engine.TxnContext{
		PaymentIntentID: "p2", CustomerID: "c1", Amount: 150_000, Country: "NK",
	})
	if r2.Decision != engine.Deny {
		t.Fatalf("p2 want Deny got %s", r2.Decision)
	}
	if got := counter.GetDaily(context.Background(), "customer:c1"); got != 200_000 {
		t.Fatalf("after p2 Deny: counter must roll back to 200000, got %d "+
			"(regression: CancelAll 没回滚 amount_limit 的预扣 → 限额槽被锁死)", got)
	}
}

// 资损 follow-up：Allow 路径下 Report() 不能对预扣过的 key 再 Incr 一次（双计）。
//
// 旧行为：Screen 通过 IncrIfBelowDaily 把 customer:c1 daily 加了 200k；Report
// 又 Incr 200k → 实际 charge 一笔 200k 但 counter 跳到 400k → 再来一笔 200k
// 撞 600k 上限被拒（其实只用了 400k）。
//
// 新行为：predebits 记录 Allow 时落过的 key，Report 命中即跳。
func TestScreen_AllowThenReport_NoDoubleCounting(t *testing.T) {
	counter := store.NewMemCounter()
	svc := newSvcWithAmountLimitRule(t, counter, 1_000_000)

	const idemKey = "idem-no-double-1"
	res := svc.Screen(context.Background(), &engine.TxnContext{
		PaymentIntentID: "pi_d1", CustomerID: "c1", Amount: 200_000,
		IdempotencyKey: idemKey,
	})
	if res.Decision != engine.Allow {
		t.Fatalf("Screen want Allow got %s", res.Decision)
	}
	// Screen 之后 daily 应该是 200k（atomic 预扣）
	if got := counter.GetDaily(context.Background(), "customer:c1"); got != 200_000 {
		t.Fatalf("after Screen: GetDaily=%d want 200000", got)
	}

	// Report 模拟 payment success
	svc.Report(context.Background(), &engine.TxnContext{
		PaymentIntentID: "pi_d1", CustomerID: "c1", Amount: 200_000,
		IdempotencyKey: idemKey,
	}, "payment.succeeded")

	// 关键断言：daily 还是 200k（不能跳到 400k）
	if got := counter.GetDaily(context.Background(), "customer:c1"); got != 200_000 {
		t.Fatalf("after Report: counter must NOT double-count, GetDaily=%d want 200000 "+
			"(regression: predebit 跟 Report Incr 没去重)", got)
	}
}

// 兜底：Report 拿到没在 Screen 里出现过的 IdempotencyKey 时走原 Incr 路径
// （比如直 Report 调用 / Screen 在另一个进程里）。
func TestReport_WithoutScreen_StillIncrements(t *testing.T) {
	counter := store.NewMemCounter()
	svc := newSvcWithAmountLimitRule(t, counter, 1_000_000)

	svc.Report(context.Background(), &engine.TxnContext{
		PaymentIntentID: "pi_x", CustomerID: "c2", Amount: 50_000,
		IdempotencyKey: "never-screened",
	}, "payment.succeeded")

	if got := counter.GetDaily(context.Background(), "customer:c2"); got != 50_000 {
		t.Fatalf("Report alone should Incr; GetDaily=%d want 50000", got)
	}
}
