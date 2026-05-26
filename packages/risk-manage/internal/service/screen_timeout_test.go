package service

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/xiongwp/risk-manage/internal/engine"
	"github.com/xiongwp/risk-manage/internal/ipintel"
	"github.com/xiongwp/risk-manage/internal/mlscore"
	"github.com/xiongwp/risk-manage/internal/store"
)

// slowIPIntel 模拟一个故意卡住的 ipintel.Service。每次 Lookup sleep d；
// 用 calls 计算实际跑了几次（验证 timeout 后 goroutine 仍可能继续，但不
// 卡住 Screen 主路径）。
type slowIPIntel struct {
	d     time.Duration
	calls atomic.Int64
}

func (s *slowIPIntel) Lookup(ctx context.Context, _ string) ipintel.Result {
	s.calls.Add(1)
	select {
	case <-time.After(s.d):
	case <-ctx.Done():
	}
	return ipintel.Result{Country: "US"}
}

// slowML 故意 sleep；ctx 取消时提前返。
type slowML struct {
	d time.Duration
}

func (m *slowML) Score(ctx context.Context, _ mlscore.Features) (mlscore.Result, error) {
	select {
	case <-time.After(m.d):
	case <-ctx.Done():
	}
	return mlscore.Result{Score: 0.42, ModelVer: "slow"}, nil
}

// newEmptyEngineSvc 构造一个最简 svc：engine 没有规则（Allow 直返），
// 没有 breaker / ml override。给 timeout 单测用。
func newEmptyEngineSvc(t *testing.T) *RiskService {
	t.Helper()
	eng := engine.New(zap.NewNop())
	if err := eng.LoadRules(nil); err != nil {
		t.Fatalf("LoadRules: %v", err)
	}
	counter := store.NewMemCounter()
	svc := New(eng, counter, zap.NewNop())
	return svc
}

// TestScreen_IPIntelTimeout_FailsOpen：ipintel sleep 100ms，IPIntel budget 20ms。
// Screen 必须在 ~20ms 后继续（不卡 100ms），verdict=Allow（无规则），
// txn.IPCountry 应保持空（fail-open 默认）。
func TestScreen_IPIntelTimeout_FailsOpen(t *testing.T) {
	svc := newEmptyEngineSvc(t)
	slow := &slowIPIntel{d: 100 * time.Millisecond}
	svc.ipIntel = slow
	svc.SetScreenTimeouts(ScreenTimeouts{
		IPIntel:        20 * time.Millisecond,
		FeatureExtract: 200 * time.Millisecond,
		MLScore:        200 * time.Millisecond,
		EngineEval:     200 * time.Millisecond,
		GlobalCeiling:  500 * time.Millisecond,
	})

	start := time.Now()
	res := svc.Screen(context.Background(), &engine.TxnContext{
		PaymentIntentID: "pi_to_ip",
		CustomerID:      "c1",
		MerchantID:      "m1",
		Amount:          10_000,
		IPAddress:       "1.2.3.4",
	})
	elapsed := time.Since(start)

	if elapsed > 80*time.Millisecond {
		t.Fatalf("Screen took %v; IPIntel budget=20ms — expected < 80ms (slow stage did not fail-open)", elapsed)
	}
	if res.Decision != engine.Allow {
		t.Fatalf("verdict=%s; expected Allow (no rules → default fail-open Allow)", res.Decision)
	}
	// fail-open 语义：IPCountry 应保留为空（ipintel 没注入），但 slowIPIntel
	// 的 goroutine 实际仍在跑（可能 100ms 后写值进 chan，但已被丢弃）。
	if got := slow.calls.Load(); got != 1 {
		t.Fatalf("slowIPIntel.calls=%d want 1", got)
	}
}

// TestScreen_MLTimeout_FailsOpen：ml sleep 200ms，MLScore budget 30ms。
// Screen 必须在 ~30ms 后继续；txn.MLScore=0（默认值）；verdict=Allow。
func TestScreen_MLTimeout_FailsOpen(t *testing.T) {
	svc := newEmptyEngineSvc(t)
	svc.mlSvc = &slowML{d: 200 * time.Millisecond}
	svc.SetScreenTimeouts(ScreenTimeouts{
		IPIntel:        200 * time.Millisecond,
		FeatureExtract: 200 * time.Millisecond,
		MLScore:        30 * time.Millisecond,
		EngineEval:     200 * time.Millisecond,
		GlobalCeiling:  500 * time.Millisecond,
	})

	txn := &engine.TxnContext{
		PaymentIntentID: "pi_to_ml",
		CustomerID:      "c1",
		MerchantID:      "m1",
		Amount:          10_000,
	}
	start := time.Now()
	res := svc.Screen(context.Background(), txn)
	elapsed := time.Since(start)

	if elapsed > 100*time.Millisecond {
		t.Fatalf("Screen took %v; ML budget=30ms — expected < 100ms", elapsed)
	}
	if res.Decision != engine.Allow {
		t.Fatalf("verdict=%s; expected Allow", res.Decision)
	}
	if txn.MLScore != 0 {
		t.Fatalf("MLScore=%v; expected 0 (fail-open default)", txn.MLScore)
	}
}

// TestScreen_AllStagesFast_ReturnsWithinCeiling：所有 stage 快 → Screen
// 必须远小于 global_ceiling。
func TestScreen_AllStagesFast_ReturnsWithinCeiling(t *testing.T) {
	svc := newEmptyEngineSvc(t)
	svc.SetScreenTimeouts(ScreenTimeouts{
		IPIntel:        20 * time.Millisecond,
		FeatureExtract: 20 * time.Millisecond,
		MLScore:        30 * time.Millisecond,
		EngineEval:     10 * time.Millisecond,
		GlobalCeiling:  100 * time.Millisecond,
	})
	start := time.Now()
	res := svc.Screen(context.Background(), &engine.TxnContext{
		PaymentIntentID: "pi_fast",
		CustomerID:      "c1",
		MerchantID:      "m1",
		Amount:          10_000,
	})
	elapsed := time.Since(start)

	if elapsed > 100*time.Millisecond {
		t.Fatalf("Screen took %v; expected < global_ceiling=100ms", elapsed)
	}
	if res.Decision != engine.Allow {
		t.Fatalf("verdict=%s; expected Allow", res.Decision)
	}
}

// TestScreen_GlobalCeilingTriggers_FailsOpen：多个 stage 各 sleep 40ms，
// 总和 > 100ms ceiling。Screen 必须在 ~ceiling 时间内返回，verdict=Allow
// （fail-open；ceiling 命中后 engine 拿到的是已 deadlined ctx，但
// engine.Evaluate 不读 ctx，仍跑完。关键是 Screen 不能卡很久）。
func TestScreen_GlobalCeilingTriggers_FailsOpen(t *testing.T) {
	svc := newEmptyEngineSvc(t)
	// IP sleep 40ms，ML sleep 40ms，加起来 80ms；但 ceiling=60ms。
	svc.ipIntel = &slowIPIntel{d: 40 * time.Millisecond}
	svc.mlSvc = &slowML{d: 40 * time.Millisecond}
	svc.SetScreenTimeouts(ScreenTimeouts{
		IPIntel:        50 * time.Millisecond,
		FeatureExtract: 50 * time.Millisecond,
		MLScore:        50 * time.Millisecond,
		EngineEval:     50 * time.Millisecond,
		GlobalCeiling:  60 * time.Millisecond,
	})
	start := time.Now()
	res := svc.Screen(context.Background(), &engine.TxnContext{
		PaymentIntentID: "pi_ceiling",
		CustomerID:      "c1",
		MerchantID:      "m1",
		Amount:          10_000,
		IPAddress:       "1.2.3.4",
	})
	elapsed := time.Since(start)

	// 容忍 50% 上浮：ceiling=60ms 时实际不应超 120ms（CI 抖动余量）
	if elapsed > 120*time.Millisecond {
		t.Fatalf("Screen took %v; ceiling=60ms — expected < ~120ms even with stage tail", elapsed)
	}
	if res == nil {
		t.Fatalf("Screen returned nil; expected fail-open Allow")
	}
	if res.Decision != engine.Allow {
		t.Fatalf("verdict=%s; expected fail-open Allow when global_ceiling triggers", res.Decision)
	}
}

// TestScreen_ZeroConfigTimeouts_NoBreakage：用 New() 构造的 svc 没调
// SetScreenTimeouts，必须保留旧行为（不破坏现有测试）。
// 即：100ms sleep 的 ipintel 不被切断（默认值 1s 远大于 100ms）。
func TestScreen_ZeroConfigTimeouts_NoBreakage(t *testing.T) {
	svc := newEmptyEngineSvc(t)
	slow := &slowIPIntel{d: 50 * time.Millisecond}
	svc.ipIntel = slow
	// 不调 SetScreenTimeouts → activeTimeouts() 返 defaults（1s per stage）

	start := time.Now()
	res := svc.Screen(context.Background(), &engine.TxnContext{
		PaymentIntentID: "pi_default",
		CustomerID:      "c1",
		MerchantID:      "m1",
		Amount:          10_000,
		IPAddress:       "1.2.3.4",
	})
	elapsed := time.Since(start)

	// 50ms sleep 应正常跑完（默认 1s budget），不应被切断
	if elapsed < 40*time.Millisecond {
		t.Fatalf("Screen took %v; expected ≥ 40ms (slowIPIntel should run to completion under default loose budget)", elapsed)
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("Screen took %v; expected < 200ms (only IP sleep adds latency)", elapsed)
	}
	if res.Decision != engine.Allow {
		t.Fatalf("verdict=%s; expected Allow", res.Decision)
	}
}

// TestScreen_TimeoutsDisabledEnv：紧急回退 env 让所有 stage timeout 失效。
func TestScreen_TimeoutsDisabledEnv(t *testing.T) {
	t.Setenv("RISK_SCREEN_TIMEOUTS_DISABLED", "1")
	svc := newEmptyEngineSvc(t)
	svc.ipIntel = &slowIPIntel{d: 30 * time.Millisecond}
	// 配极严格的 IP budget；env disable 应让它失效。
	svc.SetScreenTimeouts(ScreenTimeouts{
		IPIntel:       1 * time.Millisecond,
		GlobalCeiling: 5 * time.Millisecond, // 同样应被忽略
	})

	start := time.Now()
	res := svc.Screen(context.Background(), &engine.TxnContext{
		PaymentIntentID: "pi_disabled",
		CustomerID:      "c1",
		MerchantID:      "m1",
		Amount:          10_000,
		IPAddress:       "1.2.3.4",
	})
	elapsed := time.Since(start)

	// env disable → IP sleep 30ms 应跑完
	if elapsed < 25*time.Millisecond {
		t.Fatalf("Screen took %v; env-disabled timeouts should bypass strict 1ms budget", elapsed)
	}
	if res.Decision != engine.Allow {
		t.Fatalf("verdict=%s; expected Allow", res.Decision)
	}
}
